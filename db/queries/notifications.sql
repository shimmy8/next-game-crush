-- name: GetEmailDelivery :one
SELECT * FROM email_deliveries WHERE id=$1;

-- name: GetEmailDeliveryByIdempotencyKey :one
SELECT * FROM email_deliveries WHERE idempotency_key=$1;

-- name: InsertEmailDelivery :execrows
INSERT INTO email_deliveries(id,user_id,idempotency_key,status,kind,content_hash,recipient,subject,text_body,html_body)
VALUES($1,$2,$3,'pending',$4,$5,$6,$7,$8,$9) ON CONFLICT(idempotency_key) DO NOTHING;

-- name: ClaimEmailDelivery :one
UPDATE email_deliveries SET status='sending',attempt_worker_id=$2,attempt_token=$3,attempt_started_at=now(),error=NULL,updated_at=now()
WHERE id=$1 AND status IN ('pending','failed') RETURNING *;

-- name: MarkEmailDeliverySent :execrows
UPDATE email_deliveries SET status='sent',accepted_at=now(),confirmation_token=$3,attempt_worker_id=NULL,attempt_token=NULL,attempt_started_at=NULL,error=NULL,updated_at=now()
WHERE id=$1 AND status='sending' AND attempt_worker_id=$2 AND attempt_token=$3;
