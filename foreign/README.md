# نصب Splitd V3 روی سرور خارج

این طرف نویسندهٔ flow دانلود است و خوانندهٔ flow آپلود. روی هر اتصال TCP، payload کاربردی فقط در یک جهت حرکت می‌کند؛ تأیید دانلود از ایران در جریان آپلود و تأیید آپلود در جریان دانلود برمی‌گردد.

## مراحل

1. در `splitd.conf` مقدار `IRAN_HOST`، `TARGET`، `DL_PORT` و `UL_PORT` را تنظیم کن. DL و UL باید با سرور ایران برابر باشند و دو پورت متفاوت باشند.
2. پوشهٔ `v3/` را روی سرور خارج بگذار و اجرا کن:

```bash
cd v3/foreign
bash install.sh
```

3. پس از نصب ایران و خارج، وضعیت را ببین:

```bash
systemctl status splitd-v3-foreign
journalctl -u splitd-v3-foreign -f
```

Installer باینری `/usr/local/bin/splitd-v3` و سرویس `splitd-v3-foreign` را می‌سازد. config عملیاتی در `/etc/splitd-v3/foreign.conf` است.

`FLOW_STALE_SEC` باید با مقدار ایران برابر باشد. این آستانه فقط وقتی دادهٔ تأییدنشده گیر کرده باشد باعث تعویض اتصال‌های TCP می‌شود؛ زمان‌بندی restart فرایند نیست. با پروتکل V2 سازگار نیست و نصب هر دو سمت باید روی V3 باشد.
