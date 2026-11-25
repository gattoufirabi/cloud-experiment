# Account security

Your personal settings, the same in every organization you belong to: your profile, passkeys, recovery codes and the console's appearance.

## Overview

Open the menu at the top right of the console. It gives you:

- **Account and security**: your account page, with your name and email, your passkeys and your recovery codes.
- **Light**, **Dark** and **Auto**: the console's appearance. **Auto** follows your device's setting. The choice is remembered in this browser for a year.
- **Log out**: ends your session in this browser.

Every account must use two-step login with an authenticator app (time-based, 6-digit codes). It is set up at your first sign-in (see [Getting started](/docs/getting-started)). You can add passkeys as a faster and phishing-proof way to sign in, and recovery codes as a backup if you lose your phone.

## Use cases

- **Sign in faster**: add a passkey and use your fingerprint, face or PIN instead of a password and a code.
- **Replace a lost laptop**: remove its passkey and add one on the new device.
- **Lost phone**: sign in with your password and a recovery code, then set up two-step login again.
- **Refresh your recovery codes**: create new codes after using several, or if you think someone saw them.
- **Work comfortably**: switch to **Dark** or **Auto** appearance.

## How to use it

### Manage passkeys

A passkey lets you sign in with your fingerprint, face or device PIN instead of your password and 6-digit code. It works with phones, laptops, security keys and password managers that support passkeys.

1. Open the account menu (top right), choose **Account and security** and find **Passkeys**.
2. Enter a **Name** you will recognize (for example `Work laptop`) and click **Add a passkey**.
3. Confirm on your device (fingerprint, face, PIN or security key).
4. Next time, choose **Sign in with a passkey** on the login page and confirm on your device.

Each passkey shows when it was added and last used. A **synced** tag means the passkey lives in a password manager or cloud keychain and is available on your other devices too. Click **Remove** next to a passkey to delete it, for example a lost or replaced device.

Your password and authenticator app keep working: use them if you lose the device, then remove the lost passkey.

### Create recovery codes

Recovery codes each replace the 6-digit code once, if you lose your authenticator app.

1. On the account page, check how many unused codes you have left.
2. Click **Create recovery codes** (or **Create new codes**).
3. Save the codes in a password manager, or click **Print**.

The old codes stop working. The new codes are shown only once.

### Use a recovery code

1. On the login page, enter your email and password.
2. On the next page, type one recovery code (format `abcde-fghij`) in the **6-digit code** field, which also accepts a recovery code.
3. Soon after, set up two-step login again from your new device.

### Change the appearance

1. Open the account menu (top right).
2. Click **Light**, **Dark** or **Auto**.

### Log out

Open the account menu and click **Log out**. This ends your session in this browser only.

## Technical reference

### Sign-in methods

| Method | What you need | Notes |
| --- | --- | --- |
| Password and authenticator code | Password and 6-digit code | Required for every account. |
| Recovery code | Password and one recovery code | Each code works once. |
| Passkey | A device with a passkey | Replaces the password and the 6-digit code. |
| Single sign-on | Work email | See [Single sign-on](/docs/single-sign-on). |

### Limits and defaults

| Item | Value | Notes |
| --- | --- | --- |
| Failed passwords | 5 | The account is locked for 15 minutes. The login page says "Invalid email or password, or the account is temporarily locked." |
| Login attempts | 20 per 15 minutes per address, 10 per 15 minutes per email | Beyond that, "Too many attempts. Wait a few minutes and try again." |
| Authenticator codes | 10 per 15 minutes | |
| Recovery codes per set | 10 | Format `abcde-fghij`; each works once. |
| Appearance choice | Remembered 1 year | Stored in this browser. |

### Passkeys

| Item | Value |
| --- | --- |
| Stored on the platform | Only the public key; the private key never leaves your device or password manager |
| Phishing resistance | A passkey only works on this platform's address |
| Unsupported browsers | The page says "This browser does not support passkeys" |

### Console endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/account` | Account page. |
| POST | `/account/recovery-codes` | Create a new set of recovery codes. |
| POST | `/account/passkeys/begin` | Start adding a passkey. |
| POST | `/account/passkeys/finish` | Finish adding a passkey. |
| POST | `/account/passkeys/<id>/delete` | Remove a passkey. |
| POST | `/login/passkey/begin` | Start a passkey sign-in. |
| POST | `/login/passkey/finish` | Finish a passkey sign-in. |

## Troubleshooting

### Too many attempts

After 5 wrong passwords the account is locked for 15 minutes, and the login page says the account is temporarily locked. Too many tries in a short time also show "Too many attempts". Wait and try again.

### I lost my phone

Log in with your password and a recovery code, then set up two-step login again. If you have no codes left, ask your platform operator (see [the admin guide](/docs/admin)).

### Forgot your password

Choose **Forgot your password?** on the login page to get a one-hour link by email, or ask your operator for one. All your sessions end when you set the new password, and two-step login is still required.

### This browser does not support passkeys

Use a recent browser, or sign in with your password and authenticator code.
