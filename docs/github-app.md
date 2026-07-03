# GitHub App Setup

EPAR registers and deletes organization-level self-hosted runners through a GitHub App. The app private key stays on the host. Runner instances receive only short-lived registration tokens while they are being configured.

## Create The App

Create a GitHub App in the organization that will own the runners:

1. Open the organization settings, then create a new GitHub App.
2. Set the app name and homepage URL to values meaningful for your environment. EPAR does not receive webhooks, so no webhook URL is required.
3. Under organization permissions, grant **Self-hosted runners** read and write access.
4. Create the app, then install it into the same organization.
5. Note the numeric **App ID**.
6. Generate a private key and download the `.pem` file.

Store the private key outside Git. For this repository, `.local/github-app.pem` is a convenient ignored path.

## Configure EPAR

Set these fields in your ignored config file:

```yaml
github:
  appId: 123456
  organization: your-org
  privateKeyPath: .local/github-app.pem
  apiBaseUrl: https://api.github.com
  webBaseUrl: https://github.com
```

`github.organization` must be the organization where the app is installed. `privateKeyPath` is resolved relative to the project root unless it is absolute.

Image-only commands do not use GitHub credentials. Runner registration, GitHub-backed status, and GitHub cleanup do.

References:

- [Registering a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app)
- [Managing private keys for GitHub Apps](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps)
- [Organization self-hosted runner registration token API](https://docs.github.com/en/rest/actions/self-hosted-runners?apiVersion=2022-11-28#create-a-registration-token-for-an-organization)
