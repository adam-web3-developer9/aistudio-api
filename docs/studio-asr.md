# Studio speech recognition

The async speech recognition API uses Hailuo's AIGate workers for
`qwen3-asr-0.6b`. New API handles model permissions, wallet reservation and
task reconciliation. The client sends the audio directly to the worker.

## Channel configuration

- Create a type 64 Studio channel and list `qwen3-asr-0.6b` as its model. Use
  the Hailuo HTTP(S) base URL and the existing operator key.
- Configure a model billing expression in `billing_setting.billing_expr`.
  `u("seconds")` is the declared sum of clip durations rounded up to seconds.
  The worker checks each decoded duration before inference. ASR has no built-in
  price. Configure the expression before enabling the channel.
- Native Studio wallet reservations require Redis and batched quota updates
  to be disabled, as for H3 video.

## Request sequence

All three New API calls use the client's New API Bearer token.

1. Create with `POST /v1/audio/transcription-jobs`. Send JSON with
   `model`, optional `language` (`Chinese` or `Auto`), and `clips`.
   Each clip has `id`, `filename`, `contentType`, `sha256`, `size` in bytes,
   and `durationMs`. Send the same `Idempotency-Key` and body to retry
   an uncertain create. The response supplies the public job ID.
2. Poll `GET /v1/audio/transcription-jobs/{id}`. In `awaiting_upload`,
   `upload.files` lists `{id,url}` for the reserved worker. Use PUT to send
   the corresponding raw audio bytes to each URL. Authenticate those requests
   with `Authorization: Bearer <upload.token>` from the response.
3. Confirm with `POST /v1/audio/transcription-jobs/{id}/inputs` and body
   `{}`. Poll the same job until `succeeded` or `failed`. On success,
   `result.transcripts` contains `{id,text}` for each clip and may include
   measured `durationMs`. `workerTiming`, when present, reports worker
   timestamps in Unix milliseconds.
