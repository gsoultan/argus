package com.gsoultan.argus.helper

import android.graphics.Rect
import android.hardware.display.VirtualDisplay
import android.os.SystemClock
import android.view.InputEvent
import android.view.KeyEvent
import android.view.MotionEvent
import android.view.Surface
import org.json.JSONObject

/**
 * The hidden-framework calls the unattended engine needs, in one place.
 *
 * Every method here reaches past the public SDK into framework internals that
 * only the shell uid may call (display creation, input injection). They are
 * version-fragile by nature — the same reason scrcpy keeps a table of them per
 * Android release — so they are isolated here, behind plain signatures, and
 * this is the file that bring-up on a real device will revise. Nothing above
 * it (the protocol, the mirror and injector plumbing, the app side) depends on
 * how these are spelled.
 *
 * The app shares its screen only on a grant the Go agent verified; a wrong
 * reflection here fails a session, it does not open one.
 */
object Reflect {
    /** width, height, densityDpi of the default display. */
    fun displaySize(): IntArray {
        // SurfaceControl.getInternalDisplayToken() + getDisplayInfo is the
        // scrcpy path; DisplayManagerGlobal.getDisplayInfo(0) is the simpler
        // one and enough for size and density.
        val dmg = Class.forName("android.hardware.display.DisplayManagerGlobal")
        val get = dmg.getMethod("getInstance").invoke(null)
        val info = dmg.getMethod("getDisplayInfo", Int::class.javaPrimitiveType)
            .invoke(get, 0) ?: error("no default display")
        val cls = info.javaClass
        val w = cls.getField("logicalWidth").getInt(info)
        val h = cls.getField("logicalHeight").getInt(info)
        val dpi = cls.getField("logicalDensityDpi").getInt(info)
        return intArrayOf(w, h, dpi)
    }

    /**
     * A virtual display that mirrors the real one into surface. Created
     * through SurfaceControl so it runs as shell without the app's
     * MediaProjection prompt; AUTO_MIRROR copies the default display.
     */
    fun createMirrorDisplay(name: String, width: Int, height: Int, dpi: Int, surface: Surface): VirtualDisplay? {
        val sc = Class.forName("android.view.SurfaceControl")
        val create = sc.getMethod("createDisplay", String::class.java, Boolean::class.javaPrimitiveType)
        val token = create.invoke(null, name, false) ?: return null
        val tx = sc.getMethod("openTransaction")
        val close = sc.getMethod("closeTransaction")
        tx.invoke(null)
        try {
            sc.getMethod("setDisplaySurface", token.javaClass, Surface::class.java)
                .invoke(null, token, surface)
            sc.getMethod(
                "setDisplayProjection", token.javaClass, Int::class.javaPrimitiveType,
                Rect::class.java, Rect::class.java,
            ).invoke(null, token, 0, Rect(0, 0, width, height), Rect(0, 0, width, height))
            sc.getMethod(
                "setDisplayLayerStack", token.javaClass, Int::class.javaPrimitiveType,
            ).invoke(null, token, 0)
        } finally {
            close.invoke(null)
        }
        // The surface is driven by SurfaceControl; nothing to return but a
        // handle the caller can release. A no-op releasable keeps the plumbing
        // uniform with the attended engine's VirtualDisplay.
        return null
    }

    /** Plays one AndroidInput as a MotionEvent or KeyEvent. scale maps the
     *  console's frame coordinates back to the device. */
    fun inject(json: String, scale: Float) {
        val j = JSONObject(json)
        val x = j.optInt("x") * scale
        val y = j.optInt("y") * scale
        when (j.optString("type")) {
            "mouse" -> motion(MotionEvent.ACTION_HOVER_MOVE, x, y)
            "button" -> motion(if (j.optBoolean("down")) MotionEvent.ACTION_DOWN else MotionEvent.ACTION_UP, x, y)
            "wheel" -> scroll(x, y, j.optInt("delta"))
            "key" -> if (j.optBoolean("down")) keyFromCode(j.optString("code"))
            "device" -> deviceKey(j.optString("name"))
            "text" -> for (ch in j.optString("text")) keyChar(ch)
        }
    }

    private var downAt = 0L

    private fun motion(action: Int, x: Float, y: Float) {
        val now = SystemClock.uptimeMillis()
        if (action == MotionEvent.ACTION_DOWN) downAt = now
        val ev = MotionEvent.obtain(
            if (downAt == 0L) now else downAt, now, action, x, y, 0,
        )
        ev.source = 0x1002 // SOURCE_TOUCHSCREEN
        injectEvent(ev)
        ev.recycle()
    }

    private fun scroll(x: Float, y: Float, delta: Int) {
        // A short swipe stands in for a wheel, as the accessibility engine does.
        val d = if (delta > 0) -400f else 400f
        motion(MotionEvent.ACTION_DOWN, x, y)
        motion(MotionEvent.ACTION_MOVE, x, (y + d).coerceAtLeast(1f))
        motion(MotionEvent.ACTION_UP, x, (y + d).coerceAtLeast(1f))
    }

    private fun deviceKey(name: String) {
        val code = when (name) {
            "back" -> KeyEvent.KEYCODE_BACK
            "home" -> KeyEvent.KEYCODE_HOME
            "recents" -> KeyEvent.KEYCODE_APP_SWITCH
            "power" -> KeyEvent.KEYCODE_POWER
            "volume_up" -> KeyEvent.KEYCODE_VOLUME_UP
            "volume_down" -> KeyEvent.KEYCODE_VOLUME_DOWN
            "notifications" -> KeyEvent.KEYCODE_NOTIFICATION
            "lock" -> KeyEvent.KEYCODE_SLEEP
            else -> return
        }
        key(code)
    }

    private fun keyFromCode(code: String) {
        val k = when (code) {
            "Enter", "NumpadEnter" -> KeyEvent.KEYCODE_ENTER
            "Backspace" -> KeyEvent.KEYCODE_DEL
            "Escape" -> KeyEvent.KEYCODE_ESCAPE
            "Tab" -> KeyEvent.KEYCODE_TAB
            "Space" -> KeyEvent.KEYCODE_SPACE
            else -> {
                if (code.length == 4 && code.startsWith("Key")) {
                    keyChar(code[3].lowercaseChar())
                } else if (code.length == 6 && code.startsWith("Digit")) {
                    keyChar(code[5])
                }
                return
            }
        }
        key(k)
    }

    private fun keyChar(ch: Char) {
        val map = android.view.KeyCharacterMap.load(android.view.KeyCharacterMap.VIRTUAL_KEYBOARD)
        val events = map.getEvents(charArrayOf(ch)) ?: return
        for (e in events) injectEvent(e)
    }

    private fun key(code: Int) {
        val now = SystemClock.uptimeMillis()
        injectEvent(KeyEvent(now, now, KeyEvent.ACTION_DOWN, code, 0))
        injectEvent(KeyEvent(now, now, KeyEvent.ACTION_UP, code, 0))
    }

    private val im by lazy {
        val cls = Class.forName("android.hardware.input.InputManager")
        val get = cls.getMethod("getInstance").invoke(null)
        val inject = cls.getMethod("injectInputEvent", InputEvent::class.java, Int::class.javaPrimitiveType)
        Pair(get, inject)
    }

    private fun injectEvent(ev: InputEvent) {
        // INJECT_INPUT_EVENT_MODE_ASYNC = 0.
        im.second.invoke(im.first, ev, 0)
    }
}
