// splitd v3 is a reverse TCP tunnel over two application-layer one-way TCP flows.
//
// Foreign -> Iran (download): foreign is the only application writer.
// Iran -> foreign (upload): Iran is the only application writer.
// Cumulative acknowledgements travel on the opposite flow. A TCP connection can
// therefore be replaced without restarting either process, while unacknowledged
// frames are replayed and duplicate frames are discarded by their sequence number.
// Each individual TCP connection still carries application bytes in one direction.
//
// Wire frame: [sid:4][type:1][seq:8][length:4][payload]
// Types: OPEN=1, DATA=2, CLOSE=3, HELLO_DOWN=4, HELLO_UP=5, ACK=6.
//
// Build: GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o splitd .
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	tOpen      = 1
	tData      = 2
	tClose     = 3
	tHelloDown = 4
	tHelloUp   = 5
	tAck       = 6

	sessQ      = 512
	laneQ      = 8192
	txWindow   = 256
	maxRxAhead = 512
	maxFrame   = 1 << 20
	writeTMO   = 15 * time.Second
)

type frame struct {
	sid uint32
	typ byte
	seq uint64
	pay []byte
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func writeFrame(w io.Writer, f frame) error {
	buf := make([]byte, 17+len(f.pay))
	binary.BigEndian.PutUint32(buf[0:4], f.sid)
	buf[4] = f.typ
	binary.BigEndian.PutUint64(buf[5:13], f.seq)
	binary.BigEndian.PutUint32(buf[13:17], uint32(len(f.pay)))
	copy(buf[17:], f.pay)
	return writeAll(w, buf)
}

func readFrame(r io.Reader) (frame, error) {
	var h [17]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame{}, err
	}
	n := binary.BigEndian.Uint32(h[13:17])
	if n > maxFrame {
		return frame{}, fmt.Errorf("frame too large: %d", n)
	}
	f := frame{sid: binary.BigEndian.Uint32(h[0:4]), typ: h[4], seq: binary.BigEndian.Uint64(h[5:13])}
	if n > 0 {
		f.pay = make([]byte, n)
		if _, err := io.ReadFull(r, f.pay); err != nil {
			return frame{}, err
		}
	}
	return f, nil
}

func logf(format string, args ...any) { fmt.Fprintf(os.Stderr, "[splitd-v3] "+format+"\n", args...) }

func tune(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetNoDelay(true)
		_ = t.SetKeepAlive(true)
	}
}

// txStream keeps frames until the peer acknowledges them. A bounded in-flight
// window applies backpressure to only the affected user session.
type txStream struct {
	sid     uint32
	lane    *txLane
	mu      sync.Mutex
	next    uint64
	pending map[uint64]pendingFrame
	changed chan struct{}
}

type pendingFrame struct {
	frame frame
	at    time.Time
}

func newTxStream(sid uint32, lane *txLane) *txStream {
	return &txStream{sid: sid, lane: lane, pending: make(map[uint64]pendingFrame), changed: make(chan struct{}, 1)}
}

func (t *txStream) send(typ byte, pay []byte) {
	for {
		t.mu.Lock()
		if len(t.pending) < txWindow {
			seq := t.next
			t.next++
			f := frame{sid: t.sid, typ: typ, seq: seq, pay: pay}
			t.pending[seq] = pendingFrame{frame: f, at: time.Now()}
			t.mu.Unlock()
			t.lane.sendData(f)
			return
		}
		wait := t.changed
		t.mu.Unlock()
		<-wait
	}
}

func (t *txStream) ack(nextExpected uint64) {
	t.mu.Lock()
	if nextExpected > t.next {
		nextExpected = t.next
	}
	for seq := range t.pending {
		if seq < nextExpected {
			delete(t.pending, seq)
		}
	}
	t.mu.Unlock()
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

func (t *txStream) snapshot() []frame {
	t.mu.Lock()
	seqs := make([]uint64, 0, len(t.pending))
	for seq := range t.pending {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })
	out := make([]frame, 0, len(seqs))
	for _, seq := range seqs {
		out = append(out, t.pending[seq].frame)
	}
	t.mu.Unlock()
	return out
}

func (t *txStream) metrics() (int, time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var oldest time.Time
	for _, p := range t.pending {
		if oldest.IsZero() || p.at.Before(oldest) {
			oldest = p.at
		}
	}
	if oldest.IsZero() {
		return 0, 0
	}
	return len(t.pending), time.Since(oldest)
}

// rxStream orders frames that may arrive over overlapping old/new connections
// and discards replays. apply runs under the stream lock to preserve byte order.
type rxStream struct {
	mu     sync.Mutex
	next   uint64
	buffer map[uint64]frame
}

func (r *rxStream) accept(f frame, apply func(frame)) uint64 {
	r.mu.Lock()
	if r.buffer == nil {
		r.buffer = make(map[uint64]frame)
	}
	if f.seq >= r.next && f.seq-r.next <= maxRxAhead {
		if _, exists := r.buffer[f.seq]; !exists {
			r.buffer[f.seq] = f
		}
		for {
			cur, ok := r.buffer[r.next]
			if !ok {
				break
			}
			delete(r.buffer, r.next)
			apply(cur)
			r.next++
		}
	}
	next := r.next
	r.mu.Unlock()
	return next
}

func (r *rxStream) nextExpected() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.next
}

// txLane is the sole application writer for one TCP direction. Control frames
// (ACKs) are prioritized. Reconnection replays pending data from session state.
type txLane struct {
	name     string
	dataQ    chan frame
	controlQ chan frame
	wake     chan struct{}
	mu       sync.Mutex
	conn     net.Conn
	preface  []frame
	snapshot func() []frame
}

func newTxLane(name string, snapshot func() []frame) *txLane {
	l := &txLane{name: name, dataQ: make(chan frame, laneQ), controlQ: make(chan frame, laneQ), wake: make(chan struct{}, 1), snapshot: snapshot}
	go l.run()
	return l
}

func (l *txLane) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *txLane) setConn(c net.Conn, preface ...frame) {
	l.mu.Lock()
	old := l.conn
	l.conn = c
	l.preface = append([]frame(nil), preface...)
	l.mu.Unlock()
	if old != nil && old != c {
		_ = old.Close()
	}
	l.signal()
}

func (l *txLane) current() (net.Conn, []frame) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn, append([]frame(nil), l.preface...)
}

func (l *txLane) lost(c net.Conn) {
	l.mu.Lock()
	if l.conn == c {
		l.conn = nil
		l.preface = nil
	}
	l.mu.Unlock()
	_ = c.Close()
	l.signal()
}

func (l *txLane) rotate() {
	l.mu.Lock()
	c := l.conn
	l.mu.Unlock()
	if c != nil {
		logf("%s flow has stale unacknowledged data; replacing TCP connection", l.name)
		_ = c.Close()
	}
}

func (l *txLane) connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

func (l *txLane) sendData(f frame) {
	l.dataQ <- f
}

func (l *txLane) sendControl(f frame) {
	select {
	case l.controlQ <- f:
	default:
		// A later duplicate frame will prompt another cumulative ACK.
	}
}

func (l *txLane) write(c net.Conn, f frame) bool {
	_ = c.SetWriteDeadline(time.Now().Add(writeTMO))
	if err := writeFrame(c, f); err != nil {
		logf("%s flow write failed: %v", l.name, err)
		l.lost(c)
		return false
	}
	return true
}

func (l *txLane) run() {
	var active net.Conn
	var replay []frame
	for {
		c, preface := l.current()
		if c == nil {
			active = nil
			replay = nil
			<-l.wake
			continue
		}
		if c != active {
			active = c
			for _, f := range preface {
				if !l.write(c, f) {
					active = nil
					break
				}
			}
			if active == nil {
				continue
			}
			replay = l.snapshot()
		}
		if c != active {
			continue
		}
		select {
		case f := <-l.controlQ:
			if !l.write(c, f) {
				active = nil
			}
			continue
		default:
		}
		if len(replay) > 0 {
			f := replay[0]
			replay = replay[1:]
			if !l.write(c, f) {
				active = nil
			}
			continue
		}
		select {
		case f := <-l.controlQ:
			if !l.write(c, f) {
				active = nil
			}
		case f := <-l.dataQ:
			if !l.write(c, f) {
				active = nil
			}
		case <-l.wake:
		}
	}
}

func (l *txLane) health(streams func() []*txStream, stale time.Duration, rotate func()) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastRotate time.Time
	for range t.C {
		if !l.connected() || time.Since(lastRotate) < stale {
			continue
		}
		var oldest time.Duration
		for _, s := range streams() {
			if _, age := s.metrics(); age > oldest {
				oldest = age
			}
		}
		if oldest >= stale {
			rotate()
			lastRotate = time.Now()
		}
	}
}

func staleSeconds(flagValue int) time.Duration {
	if v := os.Getenv("FLOW_STALE_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 5 && n <= 3600 {
			return time.Duration(n) * time.Second
		}
	}
	if flagValue < 5 || flagValue > 3600 {
		flagValue = 15
	}
	return time.Duration(flagValue) * time.Second
}

// ---------------------------------------------------------------- Iran

type isess struct {
	sid          uint32
	panel        net.Conn
	downQ        chan downstreamItem
	done         chan struct{}
	closeOnce    sync.Once
	tx           *txStream
	rx           rxStream
	localClosed  atomic.Bool
	remoteClosed atomic.Bool
	closed       atomic.Bool
}

type downstreamItem struct {
	pay   []byte
	close bool
}

func (s *isess) writer() {
	for {
		select {
		case item := <-s.downQ:
			if s.closed.Load() {
				return
			}
			if item.close {
				s.closePanel()
				return
			}
			_ = s.panel.SetWriteDeadline(time.Now().Add(writeTMO))
			if _, err := s.panel.Write(item.pay); err != nil {
				s.closePanel()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *isess) closePanel() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
		_ = s.panel.Close()
	})
}

type iran struct {
	upLane  *txLane
	dlMu    sync.Mutex
	dlConn  net.Conn
	mu      sync.Mutex
	ss      map[uint32]*isess
	retired map[uint32]retiredSession
	next    atomic.Uint32
}

type retiredSession struct {
	next    uint64
	expires time.Time
}

func (i *iran) pendingUp() []frame {
	i.mu.Lock()
	streams := make([]*txStream, 0, len(i.ss))
	for _, s := range i.ss {
		streams = append(streams, s.tx)
	}
	i.mu.Unlock()
	var out []frame
	for _, s := range streams {
		out = append(out, s.snapshot()...)
	}
	return out
}

func (i *iran) txStreams() []*txStream {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]*txStream, 0, len(i.ss))
	for _, s := range i.ss {
		out = append(out, s.tx)
	}
	return out
}

func (i *iran) attachDownload(c net.Conn, r io.Reader) {
	i.dlMu.Lock()
	old := i.dlConn
	i.dlConn = c
	i.dlMu.Unlock()
	if old != nil && old != c {
		_ = old.Close()
	}
	logf("download flow attached from %s", c.RemoteAddr())
	i.serveDownload(c, r)
	i.dlMu.Lock()
	if i.dlConn == c {
		i.dlConn = nil
	}
	i.dlMu.Unlock()
}

func (i *iran) rotatePair() {
	i.upLane.rotate()
	i.dlMu.Lock()
	c := i.dlConn
	i.dlMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (i *iran) lookup(sid uint32) *isess {
	i.mu.Lock()
	s := i.ss[sid]
	i.mu.Unlock()
	return s
}

func (i *iran) cleanup(s *isess) {
	if !s.localClosed.Load() || !s.remoteClosed.Load() {
		return
	}
	if n, _ := s.tx.metrics(); n != 0 {
		return
	}
	i.mu.Lock()
	if i.ss[s.sid] == s {
		delete(i.ss, s.sid)
		i.retired[s.sid] = retiredSession{next: s.rx.nextExpected(), expires: time.Now().Add(2 * time.Minute)}
	}
	i.mu.Unlock()
}

func (i *iran) sendAck(sid uint32, next uint64) {
	i.upLane.sendControl(frame{sid: sid, typ: tAck, seq: next})
}

func (i *iran) handleDown(f frame) {
	s := i.lookup(f.sid)
	if s == nil {
		i.mu.Lock()
		r, ok := i.retired[f.sid]
		if ok && time.Now().After(r.expires) {
			delete(i.retired, f.sid)
			ok = false
		}
		i.mu.Unlock()
		if ok && f.typ != tAck {
			i.sendAck(f.sid, r.next)
		}
		return
	}
	if f.typ == tAck {
		s.tx.ack(f.seq)
		i.cleanup(s)
		return
	}
	if f.typ != tData && f.typ != tClose {
		return
	}
	next := s.rx.accept(f, func(cur frame) {
		switch cur.typ {
		case tData:
			if len(cur.pay) == 0 || s.remoteClosed.Load() || s.closed.Load() {
				return
			}
			if len(s.downQ) < sessQ {
				s.downQ <- downstreamItem{pay: cur.pay}
			} else {
				logf("session %d: panel backlog; closing this session", s.sid)
				s.remoteClosed.Store(true)
				s.downQ <- downstreamItem{close: true}
			}
		case tClose:
			if !s.remoteClosed.Swap(true) {
				s.downQ <- downstreamItem{close: true}
			}
		}
	})
	i.sendAck(f.sid, next)
	i.cleanup(s)
}

func (i *iran) serveDownload(c net.Conn, r io.Reader) {
	defer c.Close()
	for {
		f, err := readFrame(r)
		if err != nil {
			logf("download flow ended: %v", err)
			return
		}
		i.handleDown(f)
	}
}

func (i *iran) attachUpload(c net.Conn) {
	tune(c)
	i.upLane.setConn(c, frame{typ: tHelloUp, pay: []byte("splitd-v3:up")})
	logf("upload flow attached from %s", c.RemoteAddr())
	// The foreign side is intentionally silent on this flow. Read only to detect
	// closure; any peer payload violates the one-way flow contract.
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		logf("peer sent application bytes on upload flow; closing unexpected flow")
	}
	i.upLane.lost(c)
}

func (i *iran) acceptPanel(c net.Conn) {
	tune(c)
	sid := i.next.Add(1)
	s := &isess{sid: sid, panel: c, downQ: make(chan downstreamItem, sessQ+1), done: make(chan struct{})}
	s.tx = newTxStream(sid, i.upLane)
	i.mu.Lock()
	i.ss[sid] = s
	i.mu.Unlock()
	logf("session %d from %s", sid, c.RemoteAddr())
	go s.writer()
	s.tx.send(tOpen, nil)
	go func() {
		buf := make([]byte, 32768)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				cp := append([]byte(nil), buf[:n]...)
				s.tx.send(tData, cp)
			}
			if err != nil {
				break
			}
		}
		s.tx.send(tClose, nil)
		s.localClosed.Store(true)
		s.closePanel()
		i.cleanup(s)
		logf("session %d local side closed", sid)
	}()
}

func runIran(local, dl, ul string, stale time.Duration) {
	if dl == ul {
		logf("v3 requires separate download and upload ports to preserve one-way flows")
		os.Exit(2)
	}
	i := &iran{ss: map[uint32]*isess{}, retired: map[uint32]retiredSession{}}
	i.next.Store(uint32(time.Now().UnixNano()))
	i.upLane = newTxLane("upload", i.pendingUp)
	go i.upLane.health(i.txStreams, stale, i.rotatePair)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			i.mu.Lock()
			for sid, r := range i.retired {
				if time.Now().After(r.expires) {
					delete(i.retired, sid)
				}
			}
			i.mu.Unlock()
		}
	}()
	dlLn, err := net.Listen("tcp", dl)
	if err != nil {
		logf("download listen %s: %v", dl, err)
		os.Exit(1)
	}
	ulLn, e := net.Listen("tcp", ul)
	if e != nil {
		logf("upload listen %s: %v", ul, e)
		os.Exit(1)
	}
	go func() {
		for {
			c, e := ulLn.Accept()
			if e != nil {
				continue
			}
			go i.attachUpload(c)
		}
	}()
	go func() {
		for {
			c, e := dlLn.Accept()
			if e != nil {
				continue
			}
			tune(c)
			go func(c net.Conn) {
				r := bufio.NewReader(c)
				f, err := readFrame(r)
				if err != nil || f.typ != tHelloDown || string(f.pay) != "splitd-v3:down" {
					logf("download flow has invalid role preface")
					_ = c.Close()
					return
				}
				i.attachDownload(c, r)
			}(c)
		}
	}()

	ln, err := net.Listen("tcp", local)
	if err != nil {
		logf("panel listen %s: %v", local, err)
		os.Exit(1)
	}
	logf("panel on %s | download on %s | upload on %s | stale ACK threshold %s", local, dl, ul, stale)
	for {
		c, e := ln.Accept()
		if e != nil {
			continue
		}
		go i.acceptPanel(c)
	}
}

// ---------------------------------------------------------------- Foreign

type fsess struct {
	sid          uint32
	tx           *txStream
	rx           rxStream
	upQ          chan upstreamItem
	ready        chan struct{}
	done         chan struct{}
	readyOnce    sync.Once
	doneOnce     sync.Once
	tgtMu        sync.Mutex
	tgt          net.Conn
	localClosed  atomic.Bool
	remoteClosed atomic.Bool
}

type upstreamItem struct {
	pay   []byte
	close bool
}

func (s *fsess) target() net.Conn {
	s.tgtMu.Lock()
	defer s.tgtMu.Unlock()
	return s.tgt
}

func (s *fsess) setTarget(c net.Conn) bool {
	s.tgtMu.Lock()
	if s.localClosed.Load() || s.remoteClosed.Load() {
		s.tgtMu.Unlock()
		_ = c.Close()
		return false
	}
	s.tgt = c
	s.readyOnce.Do(func() { close(s.ready) })
	s.tgtMu.Unlock()
	return true
}

func (s *fsess) closeTarget() {
	if c := s.target(); c != nil {
		_ = c.Close()
	}
}

func (s *fsess) terminate() {
	s.doneOnce.Do(func() { close(s.done) })
	s.closeTarget()
}

func (s *fsess) writer() {
	for {
		select {
		case item := <-s.upQ:
			select {
			case <-s.ready:
			case <-s.done:
				return
			}
			if item.close {
				s.closeTarget()
				return
			}
			if c := s.target(); c != nil {
				_ = c.SetWriteDeadline(time.Now().Add(writeTMO))
				if _, err := c.Write(item.pay); err != nil {
					logf("session %d target write failed: %v", s.sid, err)
					s.closeTarget()
					return
				}
			}
		case <-s.done:
			return
		}
	}
}

type foreign struct {
	dlAddr string
	ulAddr string
	target string
	dialT  time.Duration

	dlLane  *txLane
	ulMu    sync.Mutex
	ulConn  net.Conn
	mu      sync.Mutex
	ss      map[uint32]*fsess
	retired map[uint32]retiredSession
}

func (f *foreign) pendingDown() []frame {
	f.mu.Lock()
	streams := make([]*txStream, 0, len(f.ss))
	for _, s := range f.ss {
		streams = append(streams, s.tx)
	}
	f.mu.Unlock()
	var out []frame
	for _, s := range streams {
		out = append(out, s.snapshot()...)
	}
	return out
}

func (f *foreign) txStreams() []*txStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*txStream, 0, len(f.ss))
	for _, s := range f.ss {
		out = append(out, s.tx)
	}
	return out
}

func (f *foreign) setUpload(c net.Conn) {
	f.ulMu.Lock()
	old := f.ulConn
	f.ulConn = c
	f.ulMu.Unlock()
	if old != nil && old != c {
		_ = old.Close()
	}
}

func (f *foreign) clearUpload(c net.Conn) {
	f.ulMu.Lock()
	if f.ulConn == c {
		f.ulConn = nil
	}
	f.ulMu.Unlock()
}

func (f *foreign) rotatePair() {
	f.dlLane.rotate()
	f.ulMu.Lock()
	c := f.ulConn
	f.ulMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (f *foreign) sendAck(sid uint32, next uint64) {
	f.dlLane.sendControl(frame{sid: sid, typ: tAck, seq: next})
}

func (f *foreign) lookup(sid uint32) *fsess {
	f.mu.Lock()
	s := f.ss[sid]
	f.mu.Unlock()
	return s
}

func (f *foreign) cleanup(s *fsess) {
	if !s.localClosed.Load() || !s.remoteClosed.Load() {
		return
	}
	if n, _ := s.tx.metrics(); n != 0 {
		return
	}
	f.mu.Lock()
	if f.ss[s.sid] == s {
		delete(f.ss, s.sid)
		f.retired[s.sid] = retiredSession{next: s.rx.nextExpected(), expires: time.Now().Add(2 * time.Minute)}
	}
	f.mu.Unlock()
	s.terminate()
}

func (f *foreign) newSession(sid uint32) *fsess {
	f.mu.Lock()
	if s := f.ss[sid]; s != nil {
		f.mu.Unlock()
		return s
	}
	s := &fsess{sid: sid, upQ: make(chan upstreamItem, sessQ+1), ready: make(chan struct{}), done: make(chan struct{})}
	s.tx = newTxStream(sid, f.dlLane)
	f.ss[sid] = s
	f.mu.Unlock()
	go s.writer()
	go func() {
		c, err := net.DialTimeout("tcp", f.target, 5*time.Second)
		if err != nil {
			logf("session %d: dial %s: %v", sid, f.target, err)
			s.tx.send(tClose, nil)
			s.localClosed.Store(true)
			s.terminate()
			f.cleanup(s)
			return
		}
		tune(c)
		if !s.setTarget(c) {
			s.tx.send(tClose, nil)
			s.localClosed.Store(true)
			s.terminate()
			f.cleanup(s)
			return
		}
		logf("session %d -> %s", sid, f.target)
		buf := make([]byte, 32768)
		for {
			n, readErr := c.Read(buf)
			if n > 0 {
				cp := append([]byte(nil), buf[:n]...)
				s.tx.send(tData, cp)
			}
			if readErr != nil {
				break
			}
		}
		s.tx.send(tClose, nil)
		s.localClosed.Store(true)
		f.cleanup(s)
		logf("session %d target side closed", sid)
	}()
	return s
}

func (f *foreign) handleUp(fr frame) {
	s := f.lookup(fr.sid)
	if s == nil {
		f.mu.Lock()
		r, ok := f.retired[fr.sid]
		if ok && time.Now().After(r.expires) {
			delete(f.retired, fr.sid)
			ok = false
		}
		f.mu.Unlock()
		if ok {
			if fr.typ != tAck {
				f.sendAck(fr.sid, r.next)
			}
			return
		}
	}
	if fr.typ == tOpen && s == nil {
		s = f.newSession(fr.sid)
	}
	if s == nil {
		return
	}
	if fr.typ == tAck {
		s.tx.ack(fr.seq)
		f.cleanup(s)
		return
	}
	if fr.typ != tOpen && fr.typ != tData && fr.typ != tClose {
		return
	}
	next := s.rx.accept(fr, func(cur frame) {
		switch cur.typ {
		case tOpen:
			// Session dialing began when OPEN arrived.
		case tData:
			if len(cur.pay) == 0 || s.remoteClosed.Load() {
				return
			}
			if len(s.upQ) < sessQ {
				s.upQ <- upstreamItem{pay: cur.pay}
			} else {
				logf("session %d: target backlog; closing this session", s.sid)
				s.remoteClosed.Store(true)
				s.upQ <- upstreamItem{close: true}
			}
		case tClose:
			if !s.remoteClosed.Swap(true) {
				s.upQ <- upstreamItem{close: true}
			}
		}
	})
	f.sendAck(fr.sid, next)
	f.cleanup(s)
}

func (f *foreign) downloadLoop() {
	backoff := 300 * time.Millisecond
	for {
		c, err := net.DialTimeout("tcp", f.dlAddr, f.dialT)
		if err != nil {
			time.Sleep(backoff)
			if backoff < 3*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 300 * time.Millisecond
		tune(c)
		if err := writeFrame(c, frame{typ: tHelloDown, pay: []byte("splitd-v3:down")}); err != nil {
			_ = c.Close()
			continue
		}
		f.dlLane.setConn(c)
		logf("download flow established to %s", f.dlAddr)
		var b [1]byte
		if _, err := c.Read(b[:]); err == nil {
			logf("peer sent application bytes on download flow; closing unexpected flow")
		}
		f.dlLane.lost(c)
		logf("download flow lost; reconnecting without process restart")
		_ = c.Close()
		time.Sleep(200 * time.Millisecond)
	}
}

func (f *foreign) uploadLoop() {
	backoff := 300 * time.Millisecond
	for {
		c, err := net.DialTimeout("tcp", f.ulAddr, f.dialT)
		if err != nil {
			time.Sleep(backoff)
			if backoff < 3*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 300 * time.Millisecond
		tune(c)
		r := bufio.NewReader(c)
		preface, err := readFrame(r)
		if err != nil || preface.typ != tHelloUp || string(preface.pay) != "splitd-v3:up" {
			logf("upload flow has invalid role preface")
			_ = c.Close()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		f.setUpload(c)
		logf("upload flow established to %s", f.ulAddr)
		for {
			fr, err := readFrame(r)
			if err != nil {
				break
			}
			f.handleUp(fr)
		}
		f.clearUpload(c)
		_ = c.Close()
		logf("upload flow lost; reconnecting without process restart")
		time.Sleep(200 * time.Millisecond)
	}
}

func runForeign(dl, ul, target string, stale time.Duration) {
	if dl == ul {
		logf("v3 requires separate download and upload ports to preserve one-way flows")
		os.Exit(2)
	}
	f := &foreign{dlAddr: dl, ulAddr: ul, target: target, dialT: 8 * time.Second, ss: map[uint32]*fsess{}, retired: map[uint32]retiredSession{}}
	f.dlLane = newTxLane("download", f.pendingDown)
	go f.dlLane.health(f.txStreams, stale, f.rotatePair)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			f.mu.Lock()
			for sid, r := range f.retired {
				if time.Now().After(r.expires) {
					delete(f.retired, sid)
				}
			}
			f.mu.Unlock()
		}
	}()
	logf("downstream -> %s | upstream <- %s | target %s | stale ACK threshold %s", dl, ul, target, stale)
	go f.downloadLoop()
	go f.uploadLoop()
	select {}
}

func main() {
	mode := flag.String("mode", "iran", "iran | foreign")
	local := flag.String("local", "127.0.0.1:93", "iran: where the panel connects")
	dl := flag.String("dl", "0.0.0.0:12797", "iran: download-flow listener (foreign writes)")
	ul := flag.String("ul", "0.0.0.0:12798", "iran: upload-flow listener (iran writes)")
	iranHost := flag.String("iran", "", "foreign: iran host")
	target := flag.String("target", "127.0.0.1:443", "foreign: local target")
	dlPort := flag.Int("dlport", 12797, "foreign: iran download-flow port")
	ulPort := flag.Int("ulport", 12798, "foreign: iran upload-flow port")
	stale := flag.Int("flow-stale", 15, "replace a flow when its oldest application frame remains unacknowledged for this many seconds")
	flag.Parse()
	staleAfter := staleSeconds(*stale)
	switch *mode {
	case "iran":
		runIran(*local, *dl, *ul, staleAfter)
	case "foreign":
		if *iranHost == "" {
			logf("-iran required")
			os.Exit(2)
		}
		dlAddr := net.JoinHostPort(*iranHost, strconv.Itoa(*dlPort))
		ulAddr := net.JoinHostPort(*iranHost, strconv.Itoa(*ulPort))
		runForeign(dlAddr, ulAddr, *target, staleAfter)
	default:
		logf("bad -mode")
		os.Exit(2)
	}
}
