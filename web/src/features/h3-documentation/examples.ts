export const h3SubmitExample = `export BASE_URL='https://ai.crypto-compass-web.online'
export API_KEY='sk-YOUR_NEW_API_KEY'
# Save this value. Reuse it only when retrying this exact request.
export REQUEST_ID='YOUR_UNIQUE_REQUEST_ID'

curl --fail-with-body -sS "$BASE_URL/v1/videos" \\
  -H "Authorization: Bearer $API_KEY" \\
  -H 'Content-Type: application/json' \\
  -H "Idempotency-Key: $REQUEST_ID" \\
  -d '{"model":"h3","prompt":"A cinematic shot of waves rolling onto a quiet beach.","seconds":"5","size":"1344x768"}'`

export const h3PollExample = `# Copy id from the submission response.
export VIDEO_ID='task_COPY_THE_RETURNED_ID'

curl --fail-with-body -sS "$BASE_URL/v1/videos/$VIDEO_ID" \\
  -H "Authorization: Bearer $API_KEY"

# Run this only after status is completed. Save before expires_at.
curl --fail-with-body -sS "$BASE_URL/v1/videos/$VIDEO_ID/content" \\
  -H "Authorization: Bearer $API_KEY" \\
  -o video.mp4`

export const h3ResponseExample = `{
  "id": "task_...",
  "object": "video",
  "model": "h3",
  "status": "queued",
  "seconds": "5",
  "size": "1344x768",
  "metadata": {
    "stage": "queued",
    "billing": {
      "unit": "second",
      "requested_seconds": 5,
      "price_per_second": 0.09,
      "currency": "USD"
    }
  }
}`

export const h3ReferenceExample = `# Python 3: pip install requests
# Set API_KEY and a NEW REQUEST_ID before running this script.
# Put reference.png and voice.wav beside this script.
import hashlib
import os
import time
from pathlib import Path
import requests

base = "https://ai.crypto-compass-web.online"
headers = {"Authorization": "Bearer " + os.environ["API_KEY"]}
request_id = os.environ["REQUEST_ID"]
media = {
    "image1": (Path("reference.png"), "image/png"),
    "audio1": (Path("voice.wav"), "audio/wav"),
}
descriptors = {
    binding: {
        "filename": path.name,
        "contentType": content_type,
        "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
    }
    for binding, (path, content_type) in media.items()
}
body = {
    "model": "h3",
    "prompt": "Animate the person in image1 speaking with audio1.",
    "seconds": "5",
    "size": "1344x768",
    "metadata": {"direct_media": descriptors},
}
response = requests.post(base + "/v1/videos", json=body,
    headers={**headers, "Idempotency-Key": request_id}, timeout=120)
response.raise_for_status()
video_id = response.json()["id"]
print("Keep this video id:", video_id)
acknowledged = set()

while True:
    response = requests.get(base + "/v1/videos/" + video_id,
        headers=headers, timeout=45)
    response.raise_for_status()
    video = response.json()
    metadata = video.get("metadata", {})
    print(video["status"], metadata.get("stage"))
    if video["status"] == "failed":
        raise RuntimeError(video.get("error"))
    if video["status"] == "completed":
        break
    upload = metadata.get("upload")
    if upload and upload["reservationId"] not in acknowledged:
        receipts = {}
        for item in upload["files"]:
            path, content_type = media[item["binding"]]
            # Deliberately no Authorization or operator key on GPU uploads.
            with path.open("rb") as source:
                sent = requests.post(upload["baseUrl"].rstrip("/") + "/upload/image",
                    files={"image": (item["filename"], source, content_type)},
                    data={"subfolder": upload["subfolder"], "type": "input", "overwrite": "true"},
                    timeout=180)
            sent.raise_for_status()
            receipt = sent.json()
            receipts[item["binding"]] = {
                "name": receipt["name"],
                "subfolder": receipt["subfolder"],
                "type": receipt["type"],
            }
        ack = requests.post(base + "/v1/videos/" + video_id + "/inputs",
            headers=headers,
            json={"reservationId": upload["reservationId"], "inputs": receipts},
            timeout=45)
        if ack.status_code == 409:
            time.sleep(3)  # Fetch the current reservation and upload again.
            continue
        ack.raise_for_status()
        acknowledged.add(upload["reservationId"])
    time.sleep(5)

with requests.get(base + "/v1/videos/" + video_id + "/content",
        headers=headers, stream=True, timeout=180) as result:
    result.raise_for_status()
    with open("video.mp4", "wb") as output:
        for chunk in result.iter_content(1024 * 1024):
            output.write(chunk)
print("Saved video.mp4")`
