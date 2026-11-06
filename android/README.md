# The Argus app for Android

The agent for an Android device. The console sees the device's screen and
drives it, with whoever holds the device accepting each session; see
`docs/install.md`, "Android devices".

The agent itself is Go: `mobile/` in the repository root, bound into
`app/libs/argus.aar` by gomobile. This project is the Kotlin shell around
it: screen capture (MediaProjection), touch (an accessibility service), the
consent question and the banner, and scanning the console's code.

## Building

```sh
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init
# With ANDROID_HOME and an NDK installed (ANDROID_NDK_HOME):
gomobile bind -target android -androidapi 29 \
    -ldflags "-X github.com/gsoultan/argus/mobile.Version=1.2.3" \
    -o android/app/libs/argus.aar ./mobile
cd android && gradle assembleDebug
```

CI does this on every change (`Agent on Android` in `.github/workflows/ci.yml`)
and keeps the APK as an artifact. A release signs it with the deployment's
own key and serves it from the control plane (`android_apk` in
`control.yaml`), or from the project's release.
