package com.gsoultan.argus

import android.content.Context
import android.util.Log
import java.io.File

/**
 * Starting the shell helper over ADB on the loopback interface — the one
 * thing an ordinary app cannot do for itself, and the boundary where this
 * feature meets a real device.
 *
 * Unattended needs code running as the `shell` user: only it may mirror the
 * display and inject input on the lock screen. The route every app-only tool
 * uses (scrcpy's app-less mode, Shizuku) is wireless debugging — the device
 * pairs with a code the user enters once, the app then speaks ADB to
 * 127.0.0.1 and runs `app_process` on the pushed helper dex. The pairing key
 * lives in the Keystore and is re-used after a reboot, which is why the engine
 * re-arms on boot rather than asking again.
 *
 * The ADB pairing handshake (SPAKE2 over TLS) and the exec channel are the
 * part of this PR that is first exercised on a device: they cannot be run
 * anywhere in CI, which has no device and no second machine to pair from. The
 * surface here is deliberately small — pair once, start the helper, stop it —
 * so bring-up revises this file and nothing above it. Until a device has
 * paired, startHelper reports why it cannot, and the app stays on the attended
 * engine rather than pretending to be unattended.
 */
class AdbClient(private val ctx: Context) {
    private val helperDex: File get() = File(ctx.filesDir, "argus-helper.dex")

    /**
     * Whether this device has paired for wireless debugging. The pairing key
     * is kept in the app's private storage (wrapped by the Keystore); its
     * presence is what "unattended is set up" means.
     */
    fun paired(): Boolean = pairingFile().exists()

    /**
     * Pairs with wireless debugging: host:port is what Developer Options shows
     * under "Pair device with pairing code", code is the six digits beside it.
     * Returns null on success or why it failed. The paired key is stored for
     * re-use after a reboot.
     */
    fun pair(host: String, port: Int, code: String): String? {
        copyHelperDex()
        // Device bring-up: the SPAKE2-over-TLS pairing exchange runs here. It
        // is written against a real adbd, which CI does not have, so it is
        // validated on a device rather than in this PR.
        return try {
            AdbPairing(ctx).pair(host, port, code, pairingFile())
            null
        } catch (e: Exception) {
            Log.w(ArgusApp.TAG, "wireless debugging pairing failed: ${e.message}")
            "pairing failed: ${e.message}"
        }
    }

    /** Pushes and runs the helper as shell. Returns null or why it could not. */
    fun startHelper(): String? {
        if (!paired()) return "this device has not paired for wireless debugging yet"
        copyHelperDex()
        return try {
            AdbExec(ctx, pairingFile()).run(
                // app_process runs the dex as the shell user; the helper then
                // opens its abstract socket for the engine to connect to.
                "CLASSPATH=${helperDex.absolutePath} app_process / com.gsoultan.argus.helper.ArgusHelper",
            )
            null
        } catch (e: Exception) {
            Log.w(ArgusApp.TAG, "could not start the helper: ${e.message}")
            "could not start the helper: ${e.message}"
        }
    }

    fun stopHelper() {
        try {
            AdbExec(ctx, pairingFile()).run("pkill -f com.gsoultan.argus.helper.ArgusHelper")
        } catch (_: Exception) {
        }
    }

    private fun pairingFile(): File = File(ctx.filesDir, "adb-pairing.key")

    private fun copyHelperDex() {
        if (helperDex.exists()) return
        ctx.assets.open("argus-helper.dex").use { src ->
            helperDex.outputStream().use { dst -> src.copyTo(dst) }
        }
    }
}
