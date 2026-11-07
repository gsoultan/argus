// The unattended helper: a small program run as the `shell` user through
// app_process, the way scrcpy's server and Shizuku run. It mirrors the
// display and injects input with shell privileges — things the app itself
// cannot do — and speaks the line protocol in Protocol.kt to the app over a
// local socket. The app pushes the built dex to the device and starts it.
//
// Built as plain classes against android.jar (compileOnly: the framework is
// on the device, not shipped here), then dexed by CI into
// app/src/main/assets/argus-helper.dex. Its pure parts are unit-tested on the
// JVM, since the framework parts can only run on a device.
plugins {
    id("org.jetbrains.kotlin.jvm")
}

kotlin {
    jvmToolchain(17)
}

dependencies {
    // The Android framework, provided by the device at run time.
    compileOnly(files("${System.getenv("ANDROID_HOME") ?: ""}/platforms/android-34/android.jar"))
    testImplementation("org.jetbrains.kotlin:kotlin-test")
    testImplementation("org.jetbrains.kotlin:kotlin-test-junit")
}

tasks.test {
    useJUnit()
}
