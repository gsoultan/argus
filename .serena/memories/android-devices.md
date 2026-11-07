# Android devices

An Android device is a managed asset whose agent is the Argus app on it:
the console sees its screen and drives it, every session is recorded and
replayed as a desktop recording. Design and mockups:
`/mnt/project-files/plans/android-devices/` (design.md, mockups/). PR 1 built
the control plane and console against a drawn stand-in; PR 2 the app in
attended mode (`mobile/` + `android/`); PR 3 the unattended engine (shell
helper over ADB, re-arm after reboot) — draft PR #178.

## How a device is stored

- `assets.kind` is `server` or `android` (migration 051). A device is stored
  the way it opens: `protocol = 'rdp'`, `desktop_mode = 'screen'`,
  `credential_mode = 'injected-password'`, no domain. `AssetInput.normalise`
  forces these for kind android, so every rdp/screen path in the console, the
  player, the live hub and Watch live works unchanged.
- `control_mode` is `attended` (whoever holds the device accepts each session
  and sees a banner) or `unattended` (the app's helper opens it with nobody
  there; wireless debugging on, for company devices).
- `control_needs_approval` makes `control` an elevated principal on that
  device: `API.elevatedOn(ctx, target, principal)` is `isElevated` plus this
  lookup, used by tickets, `/authorize` and the request-accounts list. It
  fails closed (a DB error counts as needing approval). `view` never needs one.
- A device's principals are fixed: `view` (no input, enforced on the agent
  too) and `control`. `DeviceView`/`DeviceControl` in control,
  `DeviceViewPrincipal`/`DeviceControlPrincipal` in agent,
  `DEVICE_PRINCIPALS` in the console. No accounts, so nothing personal, no
  sudo, no direct SSH.
- Enrolment: the app enrols like any agent (QR = `deviceEnrolPayload`:
  control URL, one-time token, `pin` or `off`, same rule as the install
  line); it appears under discovered hosts; `POST /discovered/{host}/enrol`
  with `kind: "android"`, `controlMode`, `controlNeedsApproval` calls
  `Store.EnrolDevice`, which stamps the row as above.

## The android service

- Agent: `internal/agent/android.go`. `AndroidService = "android"`; a grant for
  it cannot be spent on a desktop or terminal. `AndroidDevice` is what the app
  provides (`Size`, `Frame(wait)` as BGRX, `Play(AndroidInput)`, `Close`).
  `androidClient` wraps it as a `DesktopClient`: tiles via `screenDiffer`,
  recorded as Fast-Path bitmap PDUs through the tap before they are sent,
  `Refresh` repaints from the kept last frame. `DesktopInfo.Android` sets the
  recording header's protocol to `android` and the title `device@host`.
- Input from the console is `AndroidInput` JSON as the browser sends it:
  `mouse`, `button`, `wheel`, `key` (KeyboardEvent.code), `device` (name in
  `AndroidDeviceKeys`: back, home, recents, power, volume_up, volume_down,
  notifications, lock) and `text`. `runDesktop` hands these to the client's
  `Play` instead of translating to RDP events.
- `FakeAndroid` (`android_fake.go`) is a drawn phone: status bar clock ticks
  once a second, a search field shows typed text, rows light up when tapped,
  Back/Home clear. `argus-agent daemon --fake-android` offers it; the wizard's
  "No device at hand? Use a stand-in" prints the enrol + daemon lines.
- Control: `relayDesktop` picks the service from `AssetDesktopOpen(target)`
  (kind, mode): android → service `android`, password blanked, graphics
  follows video. Audit detail says the session acts as whoever holds the
  device. Session rows keep protocol `rdp`; the recording header says android.

## Console

- `kindOf` returns `'android'` from `asset.kind`; `isDevice`. Phone icon in
  lists; `connectsBy(asset)` for Overview and Settings. `isElevatedAccount
  (name, asset?)` and `principalLabel(name, asset?)` ("Watch only"/"Control")
  in `lib/roles.ts`.
- Add wizard (`/assets/add`, now "Add server or device"): Linux / Windows /
  Android; Android's step 2 is a QR (`qrcode-generator`, drawn as SVG) and the
  APK address `/api/v1/install/argus-android.apk` (served by PR 2); step 3 is
  `DeviceAccessStep`: control mode, approval switch, may watch/control, teams,
  people. `?kind=android` carries the kind when finishing later.
- `AssetForm` for a device edits name, control mode, approval and tags only.
- Connect page: no password; radios "Watch only" / "Control" with the approval
  badge from the device's setting; the button reads Watch / Open.
- `RDPScreen` takes `device`: a toolbar of device keys sending
  `{type:'device',name}`, a "Type into the device" field sending
  `{type:'text',text}`, no Ctrl+Alt+Del, the canvas capped to the window's
  height and centred. `readOnly` for `view`.
- Fixture: `rina-pixel-8` in `lib/seed.ts`.

## The app (PR 2)

- `mobile/` is the agent as the app embeds it, bound by gomobile (checked
  with `gobind -lang=java ./mobile`; the exported surface keeps to what binds).
  `Enroll(url, token, pin, hostname, stateDir)`, `ParseEnrolCode`, `Enrolled`,
  `Forget`, `NewAgent(stateDir, Device, Logger, os, model, machineID)`,
  `Agent.Start/Stop/PushFrame(rgba, w, h, stride)/EndSessions/Sessions/
  Connected`. `Device` (Kotlin) = `Consent(person, principal) bool` (blocking),
  `SessionStarted/Ended(open)`, `Play(json)`. Frames arrive RGBA and are kept
  as one BGRX frame; each session copies the newest on `Frame(wait)`.
  `openDevice` asks consent, then waits ≤10 s for the first frame.
  `AndroidConfig.Device` now takes `AndroidOpen{Person, Principal}`;
  `DesktopConfig.Connect` takes the `tunnel.Grant` (for the person's email).
  `agent.SessionReport(rec)` moved from main.go so the app reports alike.
  Tests run on Linux with a fake Device (`mobile/argus_test.go`).
- `android/`: Gradle/Kotlin, package `com.gsoultan.argus`, minSdk 29,
  AAR at `app/libs/argus.aar` (gitignored, built in CI job "Agent on Android"
  with setup-android + NDK + gomobile; APK kept as an artifact, no emulator).
  `AgentService` (foreground, specialUse|mediaProjection), `ScreenCapture`
  (VirtualDisplay → ImageReader RGBA, ≤1280 on the long side, `scale` maps
  input back), `DeviceBridge` (consent via `ConsentActivity` + the system
  capture prompt, answers matched by question id, 45 s; plus a full-screen
  notification), `InputService` (accessibility: taps/swipes/wheel, global
  actions, ACTION_SET_TEXT on the focused field, red banner overlay with Stop),
  `MainActivity` (ZXing scan or paste, enrol under the console's name).
- The QR now carries `name` (`deviceEnrolPayload(t, name)`).
- Control: `GET /api/v1/install/argus-android.apk` serves `AndroidAPK`
  (`android_apk` in control.yaml) or redirects to
  `{AgentReleasesURL}/v{offer}/argus-android.apk`; 404 for a dev build.
- Console: `kindOfHost(h)` → 'android' from `os`; "Finish adding" carries
  `kind=android`. `renderRoute(path, prepare?)` can seed the query cache.
- go.mod now requires golang.org/x/mobile (kept by `mobile/bind.go`, tools tag).

## PR 3 — the unattended engine (draft #178)

A company phone with nobody to accept a session: the screen is mirrored and
touch injected by a shell-privileged helper (scrcpy/Shizuku pattern), and a
session opens with no consent prompt because there is no one to ask.

- Agent (`mobile/argus.go`): `unattended`/`wirelessDebugging` atomic.Bool;
  `SetUnattended`/`Unattended`/`SetWirelessDebugging`. `openDevice` skips
  `device.Consent` when unattended. facts report
  `system.engine` ("attended"|"unattended") and `system.wireless_debugging`.
  Test: `TestUnattendedOpensWithNobodyToAccept`.
- Control (`internal/control/system.go`): `HostSystem` + `SystemFacts` carry
  `Engine` + `WirelessDebugging`; `bounded()` keeps engine to the two values;
  `SystemFor` passes them through. Test:
  `TestADeviceReportsItsEngineAndWirelessDebugging`.
- Console: `ServerSystemFacts.engine`/`wirelessDebugging`
  (`web/src/types/domain.ts`); `SystemCard` exports `systemQuery`; device
  Overview "About" card shows the engine + amber warning when wireless
  debugging listens on every interface (`OverviewTab.tsx` `DeviceSecurity`).
  Test: `web/src/components/__tests__/deviceOverview.test.tsx`.
- `android/helper/` (new java/kotlin.jvm module, compileOnly android.jar):
  - `Protocol.kt` — pure-JVM wire: INPUT=1, STOP=2, SIZE=16, FRAME=17,
    MAX_MESSAGE=64MB. JVM unit tests in `ProtocolTest.kt`.
  - `ArgusHelper.kt` — `main()` opens `LocalServerSocket("argus-helper")`;
    Mirror (ImageReader + Reflect display, MAX_SIDE 1280) + Injector.
  - `Reflect.kt` — hidden-API reflection: DisplayManagerGlobal, SurfaceControl
    createDisplay/setDisplaySurface/setDisplayProjection, InputManager
    injectInputEvent, MotionEvent/KeyEvent. **Device bring-up — untested here.**
  Dexed in CI (`d8 --min-api 29`) to `app/src/main/assets/argus-helper.dex`
  (gitignored; `.gitkeep` keeps the dir), run as shell user via `app_process`.
- `android/app/.../UnattendedEngine.kt`: `arm()`/`disarm()`/`setUp(host,port,
  code)`/`paired()`/`play(json)`. Connects to the helper's abstract socket
  "argus-helper"; `pump()` reads FRAME → `agent.pushFrame`. `arm()` →
  `adb.startHelper()`, `agent.setUnattended(true)`, `setWirelessDebugging(true)`.
- `AdbClient.kt` (paired/pair/startHelper/stopHelper, copies helper dex from
  assets) + `AdbTransport.kt` (`AdbPairing.pair`/`AdbExec.run` throw
  `NotYetOnDevice`). **ADB wireless-debugging pairing = SPAKE2 over TLS 1.3 +
  RSA-auth ADB message pump — the one un-validatable seam; the first on-device
  run must prove it.**
- `BootReceiver.kt`: on BOOT_COMPLETED/LOCKED_BOOT_COMPLETED, if enrolled →
  `AgentService.start` (pairing kept, so the device re-arms itself).
  `AgentService` arms off-thread when `unattended.paired() && !armed`.
  `DeviceBridge`: consent returns true when armed, play routes to
  `unattended.play`, sessionEnded skips stopCapture when unattended.
  `MainActivity` adds the unattended button + `unattendedDialog()`.
- Manifest: RECEIVE_BOOT_COMPLETED + BootReceiver. `settings.gradle.kts`
  includes `:helper`; root build adds `kotlin.jvm` 2.0.21 apply false;
  app depends on `project(":helper")`. CI: `:helper:test :helper:jar` + d8
  before assembleDebug.

## Not yet (later PRs)

4. Timeline events (app in front, lock, secure content, consent) and the Apps
   tab. 5. Linux USB relay. Also: H.264 instead of tiles, an IME for text
   beyond the focused field, an emulator smoke test, and the ADB pairing
   handshake proven on a real device.
