package com.gsoultan.argus

import android.content.Context
import java.io.File

/**
 * The ADB transport: wireless-debugging pairing and the exec channel that
 * starts the helper as shell. This is the one seam of the unattended engine
 * that no CI here can exercise — it needs a real `adbd` on a device and, for
 * pairing, a code shown on that device's screen — so it is isolated behind
 * two small classes and marked for bring-up.
 *
 * Why it is a seam and not fully inline: the pairing exchange is SPAKE2 over
 * TLS 1.3 and the connect side is an RSA-authenticated ADB message pump, the
 * same machinery scrcpy's app-less mode and Shizuku carry. It is a few hundred
 * lines of crypto that is only meaningful against a live adbd; writing it to
 * run in this PR without a device to answer would be writing it blind. So the
 * structure, the protocol, the helper, the engine lifecycle and the UI are
 * complete and tested, and this exchange is implemented on a device, where its
 * first real packet can be seen.
 *
 * Until then paired() is false, the engine stays on the attended path, and the
 * console shows the device as attended — never as unattended-but-broken.
 */
class AdbPairing(@Suppress("unused") private val ctx: Context) {
    /**
     * Pairs with the device's own adbd at host:port using the six-digit code
     * from Developer Options, and writes the resulting key to keyOut for
     * re-use after a reboot. Throws until implemented on a device.
     */
    fun pair(host: String, port: Int, code: String, keyOut: File): Unit =
        throw NotYetOnDevice("wireless-debugging pairing")
}

class AdbExec(@Suppress("unused") private val ctx: Context, @Suppress("unused") private val key: File) {
    /** Runs one shell command through the paired ADB connection on loopback. */
    fun run(command: String): Unit = throw NotYetOnDevice("the ADB exec channel")
}

/**
 * Raised by the parts of the unattended engine that can only be finished
 * against a real device. It is caught where the engine arms, so a device that
 * has not had this brought up simply reports it cannot go unattended rather
 * than crashing.
 */
class NotYetOnDevice(what: String) :
    UnsupportedOperationException("$what is implemented during device bring-up; this build cannot run it yet")
