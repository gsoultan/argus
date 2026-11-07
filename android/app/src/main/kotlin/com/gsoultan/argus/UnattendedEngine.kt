package com.gsoultan.argus

import android.content.Context
import android.net.LocalSocket
import android.net.LocalSocketAddress
import android.util.Log
import com.gsoultan.argus.helper.Protocol
import java.io.DataInputStream
import java.io.DataOutputStream
import kotlin.concurrent.thread
import mobile.Agent

/**
 * The unattended engine on the app's side: a company device with nobody to
 * accept a session, driven by the shell helper over a local socket.
 *
 * Armed, it tells the Go agent the device is unattended (so a session opens
 * without a consent prompt), starts the helper as the shell user through ADB
 * on the loopback interface (AdbClient — the device-bring-up boundary), and
 * then connects to the helper's own abstract socket on this device. From there
 * it is only plumbing: frames the helper mirrors are pushed to the agent, and
 * the console's input is sent to the helper to inject.
 *
 * Disarmed, it stops the helper and clears the unattended flag, so the device
 * falls back to the attended engine. The console's control mode is still what
 * gates approval; this only decides how the screen and touch are reached.
 */
class UnattendedEngine(private val ctx: Context, private val agent: Agent) {
    @Volatile
    var armed = false
        private set

    private var socket: LocalSocket? = null
    private var out: DataOutputStream? = null
    private var reader: Thread? = null
    private val adb = AdbClient(ctx)

    /** Whether this device has paired for wireless debugging. */
    fun paired(): Boolean = adb.paired()

    /**
     * Pairs for wireless debugging (host:port and the code from Developer
     * Options), then arms. Returns null on success or why it failed. Runs off
     * the main thread.
     */
    fun setUp(host: String, port: Int, code: String): String? {
        adb.pair(host, port, code)?.let { return it }
        return arm()
    }

    /**
     * Brings the engine up: starts the helper over ADB, connects to it, and
     * marks the agent unattended. Blocks until the helper answers or fails,
     * so callers run it off the main thread. Returns null on success or a
     * reason it could not arm.
     */
    @Synchronized
    fun arm(): String? {
        if (armed) return null
        val started = adb.startHelper()
        if (started != null) return started
        val s = LocalSocket()
        for (attempt in 1..20) {
            try {
                s.connect(LocalSocketAddress(ArgusHelperSocket, LocalSocketAddress.Namespace.ABSTRACT))
                break
            } catch (e: Exception) {
                if (attempt == 20) return "the helper did not come up: ${e.message}"
                Thread.sleep(100)
            }
        }
        socket = s
        out = DataOutputStream(s.outputStream.buffered())
        agent.setUnattended(true)
        agent.setWirelessDebugging(true)
        armed = true
        reader = thread(name = "argus-unattended") { pump(DataInputStream(s.inputStream.buffered())) }
        Log.i(ArgusApp.TAG, "unattended engine armed")
        return null
    }

    /** Runs one shell command through the helper's ADB channel, for app changes. */
    fun shell(command: String) {
        if (!armed) throw Exception("the unattended engine is not on")
        adb.shell(command)
    }

    /** As shell, with file on the command's standard input (pm install -S). */
    fun shellWithInput(command: String, file: java.io.File) {
        if (!armed) throw Exception("the unattended engine is not on")
        adb.shell(command, file)
    }

    @Synchronized
    fun disarm() {
        armed = false
        agent.setUnattended(false)
        try {
            out?.let { Protocol.writeStop(it); it.flush() }
        } catch (_: Exception) {
        }
        try {
            socket?.close()
        } catch (_: Exception) {
        }
        socket = null
        out = null
        adb.stopHelper()
        Log.i(ArgusApp.TAG, "unattended engine disarmed")
    }

    /** The agent's Device.play when unattended: send it to the helper. */
    fun play(json: String) {
        val o = out ?: throw IllegalStateException("the unattended helper is not connected")
        synchronized(o) {
            Protocol.writeInput(o, json)
            o.flush()
        }
    }

    private fun pump(input: DataInputStream) {
        try {
            while (armed) {
                val m = Protocol.read(input) ?: break
                when (m.type) {
                    Protocol.FRAME -> {
                        val w = m.int(0)
                        val h = m.int(4)
                        val stride = m.int(8)
                        agent.pushFrame(m.body.copyOfRange(12, m.body.size), w.toLong(), h.toLong(), stride.toLong())
                    }
                    Protocol.SIZE -> Log.i(ArgusApp.TAG, "device screen ${m.int(0)}x${m.int(4)}")
                }
            }
        } catch (e: Exception) {
            if (armed) Log.w(ArgusApp.TAG, "unattended stream ended: ${e.message}")
        }
    }

    companion object {
        /** The abstract socket name the helper listens on (ArgusHelper.SOCKET). */
        const val ArgusHelperSocket = "argus-helper"
    }
}
