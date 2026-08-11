# HomeForge Push Relay

A tiny, **stateless** service that forwards notifications to Firebase Cloud Messaging (FCM).

Android/iOS require background push to go through FCM/APNs, tied to a Firebase project + a
*private* sender identity. For a free, self-hosted, open-source app you can't ship that sender
key publicly — so, like the Home Assistant companion app, one relay holds the sender identity
and every self-hoster's HomeForge backend just POSTs to it. Self-hosters need **zero Firebase
setup**; the app ships a public Firebase client config.

## Design
- **Stateless** — stores nothing. Device tokens live on each HomeForge backend. Scale/move freely.
- **No key file** — on Cloud Run it authenticates to FCM via the attached service account (ADC).
- **Rate-limited** — 60 req/min per IP as an abuse backstop.

## API
`POST /push`
```json
{
  "tokens": ["<fcm-token>", "..."],
  "title": "Front Door",
  "message": "Someone is at the front door",
  "image": "https://.../snapshot.jpg",   // optional
  "channel": "doorbell",                   // optional Android channel id
  "tag": "front_door",                     // optional collapse key
  "data": { "click": "camera:front_door" } // optional
}
```
Response: `{ "success": N, "failure": M, "invalid_tokens": [...] }` — prune `invalid_tokens`
from your backend.

`GET /health` → `ok`  (note: `/healthz` is swallowed by the Cloud Run front end, so this uses `/health`)

## Deploy (Cloud Run, no key file)
```sh
gcloud run deploy homeforge-push --source . --region us-central1 \
  --service-account homeforge-push@<project>.iam.gserviceaccount.com \
  --allow-unauthenticated --max-instances 3 --project <project>
```
The service account needs `roles/firebasecloudmessaging.admin` on the project.
