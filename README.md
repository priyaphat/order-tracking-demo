# Order Tracking API (Back-End Developer Test)

โปรเจกต์ย่อยสำหรับโจทย์ระบบติดตามออเดอร์ร้านเติมเกม เน้น backend correctness มากกว่า UI และแยกจากระบบ PKP หลักเพื่อไม่ให้ domain `production_orders`/LAB ปะปนกับออเดอร์เติมเกม

## Stack และสิ่งที่มีให้

- Go 1.24 + Chi, PostgreSQL 16 + pgx, Goose migration
- Docker Compose รัน API และฐานข้อมูลด้วยคำสั่งเดียว
- REST API + OpenAPI + [`requests.http`](requests.http)
- Idempotent create, pagination, order-number validation และ error response กลาง
- Explicit state machine และ append-only timeline
- Atomic bot claim, lease/heartbeat, retry/backoff, signed webhook และ background sweeper
- Unit tests สำหรับ validation, state machine และ retry policy
- Structured JSON log พร้อม `request_id` และ `order_no`

จำนวนเงินเก็บด้วย PostgreSQL `numeric(18,2)` และส่งผ่าน JSON เป็น string เพื่อไม่ใช้ floating point เป็น source of truth

## เริ่มใช้งาน

ต้องมี Docker Desktop จากโฟลเดอร์นี้รัน:

```bash
docker compose up --build
```

API: `http://localhost:8088`  
OpenAPI: `http://localhost:8088/openapi.yaml`  
PostgreSQL: `127.0.0.1:5438` เท่านั้น

ตรวจสอบ:

```bash
go test ./...
go vet ./...
```

ค่า local สำหรับ internal endpoint คือ `X-Internal-Token: local-worker-token` และ webhook secret คือ `local-bank-secret` เปลี่ยนได้ผ่าน `.env` (ดู `.env.example`) ห้ามใช้ค่า local ใน production

## API หลัก

| Method | Path | รายละเอียด |
| --- | --- | --- |
| POST | `/api/v1/orders` | สร้างออเดอร์; บังคับ `Idempotency-Key` |
| GET | `/api/v1/orders/{orderNo}` | format ผิดตอบ 400, format ถูกแต่ไม่พบตอบ 404 |
| GET | `/api/v1/orders?tab=current\|history&page=1&pageSize=20` | ใหม่ไปเก่าและแบ่งหน้า |
| GET | `/api/v1/orders/{orderNo}/timeline` | เหตุการณ์แบบ append-only |
| POST | `/api/v1/orders/{orderNo}/report-issue` | ลูกค้าแจ้งปัญหา |

Internal API ใน OpenAPI ใช้จำลอง bot/bank/admin ได้แก่ claim, heartbeat, result, แก้ UID แล้ว requeue, bank webhook และ manual sweep

Error ทุก endpoint เป็นรูปเดียวกัน:

```json
{
  "code": "invalid_order_no",
  "message": "order number must match ORD-<13 digits>-<3 digits>",
  "requestId": "c5d..."
}
```

## State machine

ทางปกติคือ `PENDING_PAYMENT -> QUEUED -> IN_PROGRESS -> COMPLETED`

- `DELAYED`: retry ครบหรือรอตรวจสอบโดยคน
- `FAILED`: permanent error เช่น UID ผิด
- `EXPIRED`: ไม่จ่ายก่อน QR/payment window หมด
- `REFUNDED`: คืนเงินจาก terminal/problem state หรือรับเงินมาช้าหลังหมดอายุ

Transition ถูกตรวจใน service ไม่ให้ข้ามจาก `PENDING_PAYMENT` ไป `COMPLETED` หรือให้ worker ที่เสีย lease แล้วเขียนผลทับ worker ใหม่

## เหตุผลการแก้เคสปัญหา 5 ข้อ

### 1. Bot ล่ม/cookie หมด/timeout

เมื่อ bot claim งาน ระบบบันทึก `lease_owner`, `lease_expires_at` และ heartbeat Bot ต้องต่อ lease เป็นระยะ ถ้าเงียบ sweeper จะนำกลับ `QUEUED` ด้วย exponential backoff 5s, 10s, 20s (ปรับได้) เมื่อครบ `MAX_RETRIES` จะย้ายเป็น `DELAYED` และบันทึก timeline วิธีนี้รองรับ process crash เพราะ lease อยู่ใน PostgreSQL ไม่ได้อยู่ใน memory

### 2. Bot สองตัวแย่งงานเดียวกัน

Claim ใช้ transaction และ `SELECT ... FOR UPDATE SKIP LOCKED` จึงมี bot เดียว lock/claim แถวนั้นได้ พร้อมตรวจ active lease ทุก heartbeat/result ตาราง `payment_attempts` มี `UNIQUE(order_id, attempt_no)` เป็น safety net ป้องกัน attempt เดิมซ้ำ

### 3. Webhook ธนาคารซ้ำ ช้า หรือผิดลำดับ

ตรวจ HMAC-SHA256 ก่อนอ่านผลทางธุรกิจ และเก็บ `UNIQUE(source,event_id)` ถ้า event ซ้ำจะ replay ผลเดิมโดยไม่เปลี่ยน state ซ้ำ การเปลี่ยน state ทำใน transaction พร้อม lock order; paid ที่มาหลัง `EXPIRED` จะเป็น `REFUNDED` ส่วน event ที่ผิดลำดับจะถูกเก็บเป็น `ignored_out_of_order` เพื่อ audit

### 4. เติมไม่สำเร็จ

Bot ส่ง outcome เป็น `transient_failure` หรือ `permanent_failure`: transient เข้า retry/backoff ส่วน permanent ไป `FAILED` ทันทีและเก็บ error code ใน timeline มี internal endpoint แก้ `playerUid` และ requeue สำหรับเคส UID ผิด โดยไม่แก้ประวัติเดิม

### 5. ออเดอร์ค้าง/หมดอายุ

Background sweeper รันทุก `SWEEP_INTERVAL` และเขียนแบบมี condition จึงรันซ้ำได้: ปิด `PENDING_PAYMENT` ที่เกิน `expires_at`, กู้ lease ที่หมด และสร้าง queue SLA alert ด้วย partial unique index เพื่อไม่สร้าง alert ซ้ำ มี manual endpoint สำหรับ demo/operational run ด้วย

## ข้อควรทำต่อก่อน production

- วาง internal API หลัง private network/service identity หรือ mTLS และเก็บ secret ใน secret manager
- เพิ่ม integration test ด้วย PostgreSQL จริงใน CI โดยเฉพาะ concurrent claim/webhook
- ส่ง `order_alerts` ผ่าน outbox ไป Slack/PagerDuty และเพิ่ม metrics/tracing
- เพิ่ม authentication/rate limit/ownership check สำหรับ customer-facing endpoints
- ทำ backup/restore drill และกำหนด retention ก่อนใช้ข้อมูลจริง

ไม่มี UI ในโปรเจกต์ย่อยนี้โดยตั้งใจ เพราะโจทย์วัด backend และสามารถทดลอง flow ทั้งหมดจาก `requests.http` ได้
