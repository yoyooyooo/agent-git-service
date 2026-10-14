# GitHub App registration

Registration is an opt-in operator capability, separate from the PAT-backed GitHub projection. It is disabled by default. A successful conversion does not enable projections or prove installation/webhook delivery.

Configure `github_app.registration_enabled: true`, `registration_state_file`, `registration_redirect_url`, and the three existing credential-file paths in the integrations YAML. Every registration file must have a distinct absolute path in an existing process-owned mode-0700 parent directory. The redirect URL must be an HTTPS `.ts.net` origin with the exact path `/api/ext/v1/integrations/github/app-manifest/callback`, without query, userinfo or fragment. Existing credentials remain mode 0600. Keep `github_app.enabled: false` until the new credential set is committed; registration can begin without those files existing.

```yaml
github_app:
  enabled: false
  registration_enabled: true
  registration_state_file: /srv/ags/config/github-app/registration-state.json
  registration_redirect_url: https://example.ts.net/api/ext/v1/integrations/github/app-manifest/callback
  app_id_file: /srv/ags/config/github-app/app-id
  private_key_file: /srv/ags/config/github-app/private-key.pem
  webhook_secret_file: /srv/ags/config/github-app/webhook-secret
```

Back up operator configuration before changing it. Deploy reviewed code through the owning service workflow; source merge does not update a running process. This setup must not be confused with an accepted live deployment.

The owner performs these browser actions after the reviewed runtime/configuration is active:

1. Use a native site-admin credential file and run `python3 scripts/github-app-registration.py --api-url https://example.ts.net --token-file /private/operator.token --name 'Example AGS' --output /private/github-app-registration.html`. The helper calls authenticated `POST /api/ext/v1/integrations/github/app-manifest/start` and writes a new mode-0600 form. It prints only the form path, not the credential, one-time state or manifest code. Use a new output path; never upload the form.
2. Open that local HTML file in the owner's signed-in browser. Click **Review and create on GitHub**. It POSTs the manifest to `https://github.com/settings/apps/new` with the one-hour state carried by the generated form.
3. On GitHub, review the app name and repository permissions, then click **Create GitHub App**. Keep the browser connected to the tailnet; GitHub redirects it to the exact callback URL above. Do not copy the callback code/state into a chat or log. The callback commits the App ID, RSA key and webhook secret, then independently authenticates `GET /app` with an RS256 JWT. It returns only app identity and an installation URL.
4. Open the returned `installation_url`, whose exact form is `https://github.com/apps/<returned-slug>/installations/new`. Select the intended owner, choose **Only select repositories**, choose the repositories to authorize, and click **Install**. A real installation is a separate owner action; do not infer it from conversion or a fixture test.

Authenticated site-admin `GET /api/ext/v1/integrations/github/app-manifest/app` rereads the saved credential files and probes GitHub App identity. The integration does not retain a registration conversion as runtime credentials. A callback with already-saved credentials but an unavailable identity probe returns `202 / credentials_saved_app_probe_pending`; use the status route after recovery, rather than repeating the one-time conversion.

A missing/mismatched/expired state or repeated code fails closed. Query code/state and provider credential bodies are never returned or logged. Anonymous/non-admin users cannot start registration or read app identity. The state authenticates the public browser callback; the start route requires a native authenticated administrator.

The generated manifest keeps webhook delivery **inactive** because GitHub cannot call a private tailnet receiver. Activating delivery requires a separately reviewed reachable ingress. The installation step above does not change that boundary.

[Module ownership](../module-contracts.md#githubappmanifest) · [Documentation navigation](../README.md)
