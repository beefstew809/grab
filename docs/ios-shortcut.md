# iPhone: share to grab with a Shortcut

iOS does not offer installed web apps in the share sheet, so on an iPhone you
use a Shortcut that sends the link to grab's API.

## Before you start

1. Open `<BASE_URL>/?settings` in Safari, sign in, and create an **API token**.
   It starts with `grab_` and is shown once; copy it.
2. Note your grab address (`BASE_URL`), for example `https://grab.example.com`.

## The request

```
POST <BASE_URL>/api/downloads
Authorization: Bearer grab_...
Content-Type: application/json

{"url": "<the shared link>", "target": "telegram"}
```

`target` is optional: `original` (download only), `telegram`, `signal`,
`messages` or `discord`. Leave it out to use the default you chose in grab's
settings. The response is `201` for a new download and `200` if it already
exists.

## Build the Shortcut

1. Shortcuts app, **+** to create a new shortcut. Name it "grab".
2. Open the shortcut's details (the **i** button) and turn on **Show in Share
   Sheet**. Under accepted types keep **URLs** (and **Safari web pages** if
   you like) and turn the rest off.
3. Add **Get URLs from Input** (input: Shortcut Input).
4. Add **Get Contents of URL**:
   - URL: `<BASE_URL>/api/downloads`
   - Method: **POST**
   - Headers: `Authorization` = `Bearer grab_...` (your token)
   - Request Body: **JSON**
   - Add a field `url` of type Text, value the **URLs** variable from step 3.
   - (Optional) add a field `target` of type Text, for example `telegram`.
5. Add **Show Notification** (or **Show Result**) so you can see it worked.

Share a link from Safari or any app, choose **grab**, and it is queued.

## Variant: choose the target each time

To pick where the file is going every time you share:

1. After **Get URLs from Input**, add **Choose from Menu** with the prompt
   "Send to" and the items `Download only`, `Telegram`, `Signal`, `Messages`
   and `Discord`.
2. Under each menu item add a **Text** action containing the matching value
   (`original`, `telegram`, `signal`, `messages`, `discord`), and a
   **Set Variable** action named `target` after it.
3. In **Get Contents of URL**, set the `target` field to the `target`
   variable.

## Tips

- Treat the token like a password. If it leaks, delete it on the settings page
  and make a new one.
- A token keeps working until you revoke it, even if your account is later
  removed from the allowed group at your identity provider; see the README
  section "When access changes take effect".
