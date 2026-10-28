# Android automation: HTTP Shortcuts

If you would rather not install the PWA, or want a one-tap button, the
[HTTP Shortcuts](https://http-shortcuts.rmy.ch/) app can send a shared link to
grab. Any other HTTP client works the same way.

## Before you start

Open `<BASE_URL>/?settings` in a browser, sign in, and create an **API token**
(it starts with `grab_` and is shown once).

## The request

```
POST <BASE_URL>/api/downloads
Authorization: Bearer grab_...
Content-Type: application/json

{"url": "<the shared link>", "target": "telegram"}
```

`target` is optional (`original`, `telegram`, `signal`, `messages`,
`discord`); without it grab uses your default from the settings page. A `201`
means a new download, `200` means it already existed.

## Set it up

1. In HTTP Shortcuts create a new shortcut ("Regular HTTP Shortcut").
2. Method **POST**, URL `<BASE_URL>/api/downloads`.
3. Header: `Authorization` = `Bearer grab_...`.
4. Body type **Custom text**, content type `application/json`, body:

   ```
   {"url": "{shared_url}"}
   ```

   Create `shared_url` as a variable of type **Text**, and tick
   **Allow receiving value from Share Dialog** so the shared link fills it.
   Also turn on the variable's **JSON-encode** option (in its encoding
   settings). Apps often share text rather than a bare link, and a quote or
   line break in that text inserted raw would break the JSON body; encoded,
   grab gets the text intact and finds the link in it.
5. In the shortcut's settings enable **Show in Share Dialog** (named
   "Allow receiving data from other apps" in some versions).
6. To choose the target each time, add a second variable `target` of type
   **Selection** with the values `original`, `telegram`, `signal`,
   `messages`, `discord`, and use this body instead:

   ```
   {"url": "{shared_url}", "target": "{target}"}
   ```

Share a link, pick the shortcut, and grab starts the download.

Treat the token like a password; you can revoke it on the settings page at any
time.
