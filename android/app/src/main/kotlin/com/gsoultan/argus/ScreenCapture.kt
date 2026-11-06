package com.gsoultan.argus

import android.content.Context
import android.graphics.PixelFormat
import android.hardware.display.DisplayManager
import android.hardware.display.VirtualDisplay
import android.media.ImageReader
import android.media.projection.MediaProjection
import android.os.Handler
import android.os.HandlerThread
import android.util.DisplayMetrics
import android.util.Log
import android.view.WindowManager
import mobile.Agent

/**
 * The screen as frames: a virtual display mirrors it into an ImageReader,
 * and every image is handed to the agent as RGBA, which turns it into the
 * tiles the console draws (internal/agent/android.go).
 *
 * Captured at most 1280 pixels on the long side: a phone's own resolution
 * is more than the console shows, and every pixel costs on the tunnel and
 * in the recording. Input from the console is in these frame coordinates;
 * scale() maps it back to the screen.
 */
class ScreenCapture(private val ctx: Context, private val projection: MediaProjection, private val agent: Agent) {
    private val thread = HandlerThread("argus-capture").also { it.start() }
    private val handler = Handler(thread.looper)
    private var reader: ImageReader? = null
    private var display: VirtualDisplay? = null

    @Volatile
    var width = 0
        private set

    @Volatile
    var height = 0
        private set

    @Volatile
    var scale = 1f
        private set

    private var buf = ByteArray(0)

    fun start() {
        handler.post { open() }
    }

    fun resize() {
        handler.post {
            close()
            open()
        }
    }

    fun stop() {
        handler.post { close() }
        thread.quitSafely()
    }

    private fun open() {
        val wm = ctx.getSystemService(Context.WINDOW_SERVICE) as WindowManager
        val metrics = DisplayMetrics()
        @Suppress("DEPRECATION")
        wm.defaultDisplay.getRealMetrics(metrics)
        val sw = metrics.widthPixels
        val sh = metrics.heightPixels
        val s = minOf(1f, MAX_SIDE.toFloat() / maxOf(sw, sh))
        // Even sizes: some encoders and the tile grid prefer them.
        width = ((sw * s).toInt() / 2) * 2
        height = ((sh * s).toInt() / 2) * 2
        scale = sw.toFloat() / width
        val r = ImageReader.newInstance(width, height, PixelFormat.RGBA_8888, 2)
        r.setOnImageAvailableListener({ onImage(it) }, handler)
        reader = r
        display = projection.createVirtualDisplay(
            "argus", width, height, metrics.densityDpi,
            DisplayManager.VIRTUAL_DISPLAY_FLAG_AUTO_MIRROR, r.surface, null, handler,
        )
        Log.i(ArgusApp.TAG, "capturing ${width}x${height} of ${sw}x${sh}")
    }

    private fun close() {
        display?.release()
        display = null
        reader?.close()
        reader = null
    }

    private fun onImage(r: ImageReader) {
        val img = r.acquireLatestImage() ?: return
        try {
            val plane = img.planes[0]
            val stride = plane.rowStride
            val bb = plane.buffer
            val n = bb.remaining()
            if (buf.size < n) buf = ByteArray(n)
            bb.get(buf, 0, n)
            // Handed over by copy: the agent keeps its own.
            agent.pushFrame(buf, img.width.toLong(), img.height.toLong(), stride.toLong())
        } finally {
            img.close()
        }
    }

    companion object {
        const val MAX_SIDE = 1280
    }
}
