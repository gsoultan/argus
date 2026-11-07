package com.gsoultan.argus.helper

import android.graphics.PixelFormat
import android.hardware.display.VirtualDisplay
import android.media.ImageReader
import android.os.Handler
import android.os.Looper
import android.net.LocalServerSocket
import android.net.LocalSocket
import java.io.DataInputStream
import java.io.DataOutputStream

/**
 * The unattended engine, run as the `shell` user through app_process — the
 * same trick scrcpy's server and Shizuku use. With shell privileges it can
 * mirror the display and inject input without the accessibility service or a
 * per-session capture prompt, which is what "nobody holding the device" needs.
 *
 * It opens an abstract local socket the app connects to on the same device,
 * mirrors the display into it as RGBA frames, and plays the input the app
 * sends. All of the framework calls go through reflection in Mirror and
 * Injector, so this file carries only the plumbing; the framing it speaks is
 * Protocol, which is tested on the JVM.
 *
 * It holds nothing and decides nothing: a session still opens only on a grant
 * the Go agent verified against the pinned control-plane key. The helper is a
 * screen and a finger, not a gate.
 */
object ArgusHelper {
    const val SOCKET = "argus-helper"
    const val MAX_SIDE = 1280

    @JvmStatic
    fun main(args: Array<String>) {
        // app_process has no Looper; the display mirror needs one.
        if (Looper.myLooper() == null) Looper.prepareMainLooper()
        val server = LocalServerSocket(SOCKET)
        log("listening on @$SOCKET")
        while (true) {
            val socket = try {
                server.accept()
            } catch (e: Exception) {
                log("accept failed: ${e.message}")
                break
            }
            try {
                serve(socket)
            } catch (e: Exception) {
                log("session ended: ${e.message}")
            } finally {
                try {
                    socket.close()
                } catch (_: Exception) {
                }
            }
        }
    }

    private fun serve(socket: LocalSocket) {
        val out = DataOutputStream(socket.outputStream.buffered())
        val input = DataInputStream(socket.inputStream.buffered())
        val injector = Injector()
        val mirror = Mirror(MAX_SIDE)

        val sent = Object()
        mirror.open { width, height, stride, rows, rowsLen ->
            synchronized(sent) {
                Protocol.writeFrame(out, width, height, stride, rows, rowsLen)
                out.flush()
            }
        }
        synchronized(sent) {
            Protocol.writeSize(out, mirror.width, mirror.height)
            out.flush()
        }
        injector.scale = mirror.scale
        try {
            while (true) {
                val m = Protocol.read(input) ?: break
                when (m.type) {
                    Protocol.INPUT -> injector.play(m.text())
                    Protocol.STOP -> break
                }
            }
        } finally {
            mirror.close()
        }
    }

    fun log(s: String) {
        System.err.println("argus-helper: $s")
    }
}

/**
 * The display, mirrored with shell privileges. DisplayManager's hidden
 * createVirtualDisplay is reachable from the shell uid, which an ordinary app
 * cannot call; reflection keeps this compiling against the public SDK. On a
 * device where that path is refused the mirror reports nothing and the app's
 * session ends with "no screen", as the fake does when told to share nothing.
 */
class Mirror(private val maxSide: Int) {
    var width = 0
        private set
    var height = 0
        private set
    var scale = 1f
        private set

    private var reader: ImageReader? = null
    private var display: VirtualDisplay? = null
    private var buf = ByteArray(0)

    fun interface Sink {
        fun frame(width: Int, height: Int, stride: Int, rows: ByteArray, rowsLen: Int)
    }

    fun open(sink: Sink) {
        val fm = Reflect.displaySize()
        val sw = fm[0]
        val sh = fm[1]
        val dpi = fm[2]
        val s = minOf(1f, maxSide.toFloat() / maxOf(sw, sh))
        width = ((sw * s).toInt() / 2) * 2
        height = ((sh * s).toInt() / 2) * 2
        scale = if (width == 0) 1f else sw.toFloat() / width
        val r = ImageReader.newInstance(width, height, PixelFormat.RGBA_8888, 2)
        val handler = Handler(Looper.getMainLooper())
        r.setOnImageAvailableListener({ reader ->
            val img = reader.acquireLatestImage() ?: return@setOnImageAvailableListener
            try {
                val plane = img.planes[0]
                val bb = plane.buffer
                val n = bb.remaining()
                if (buf.size < n) buf = ByteArray(n)
                bb.get(buf, 0, n)
                sink.frame(img.width, img.height, plane.rowStride, buf, n)
            } finally {
                img.close()
            }
        }, handler)
        reader = r
        display = Reflect.createMirrorDisplay("argus", width, height, dpi, r.surface)
    }

    fun close() {
        display?.release()
        reader?.close()
    }
}

/**
 * Input injection with shell privileges: InputManager.injectInputEvent, which
 * the shell uid may call for any window, including the lock screen — the whole
 * point of the unattended engine over the accessibility service. Reflection
 * again, so this builds against the public SDK. The console's input comes in
 * as the same AndroidInput JSON the attended engine plays.
 */
class Injector {
    var scale = 1f

    fun play(json: String) {
        Reflect.inject(json, scale)
    }
}
