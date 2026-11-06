package com.gsoultan.argus

import android.accessibilityservice.AccessibilityService
import android.accessibilityservice.GestureDescription
import android.content.Context
import android.graphics.Color
import android.graphics.PixelFormat
import android.graphics.Path
import android.media.AudioManager
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import android.util.Log
import android.view.Gravity
import android.view.WindowManager
import android.view.accessibility.AccessibilityEvent
import android.view.accessibility.AccessibilityNodeInfo
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView
import kotlin.math.abs
import kotlin.math.hypot

/**
 * How the console touches the device: an accessibility service, which is
 * what Android offers an app for touching the screen without root.
 *
 * Input arrives as the console sends it (internal/agent/android.go): a
 * press and release make a tap, a press moved then released a swipe along
 * the way, a wheel a short swipe, typed text goes into the field that has
 * focus, and the device keys are the global actions.
 *
 * The service also draws the banner a session shows, as an accessibility
 * overlay: that needs no further permission, and whoever holds the device
 * sees who is on it and can stop it.
 */
class InputService : AccessibilityService() {
    private var downX = 0f
    private var downY = 0f
    private var downAt = 0L
    private var path: Path? = null
    private var last = 0L
    private var banner: LinearLayout? = null

    override fun onServiceConnected() {
        super.onServiceConnected()
        instance = this
        Log.i(ArgusApp.TAG, "accessibility service on")
    }

    override fun onDestroy() {
        instance = null
        banner(null)
        super.onDestroy()
    }

    override fun onAccessibilityEvent(event: AccessibilityEvent?) {}

    override fun onInterrupt() {}

    /* ── Touch ──────────────────────────────────────────────────────────── */

    fun pointerMoved(x: Float, y: Float) {
        val p = path ?: return
        val now = SystemClock.uptimeMillis()
        // Keep the swipe's shape, not every pixel of it.
        if (now - last < 16) return
        last = now
        p.lineTo(x, y)
    }

    fun pointerButton(x: Float, y: Float, down: Boolean) {
        if (down) {
            downX = x
            downY = y
            downAt = SystemClock.uptimeMillis()
            path = Path().also { it.moveTo(x, y) }
            return
        }
        val p = path ?: return
        path = null
        val held = SystemClock.uptimeMillis() - downAt
        if (hypot((x - downX).toDouble(), (y - downY).toDouble()) < TAP_SLOP) {
            // A tap, or a long press held long enough to be one.
            tap(downX, downY, if (held > LONG_PRESS_MS) held else TAP_MS)
            return
        }
        p.lineTo(x, y)
        stroke(p, held.coerceIn(MIN_SWIPE_MS, MAX_SWIPE_MS))
    }

    fun wheel(x: Float, y: Float, delta: Int) {
        // Scrolling down moves the content up: a swipe up from the pointer.
        val d = if (delta > 0) -SCROLL_PX else SCROLL_PX
        val p = Path()
        p.moveTo(x, y)
        p.lineTo(x, (y + d).coerceAtLeast(1f))
        stroke(p, 120)
    }

    private fun tap(x: Float, y: Float, ms: Long) {
        val p = Path()
        p.moveTo(x, y)
        stroke(p, ms)
    }

    private fun stroke(p: Path, ms: Long) {
        val g = GestureDescription.Builder()
            .addStroke(GestureDescription.StrokeDescription(p, 0, ms))
            .build()
        if (!dispatchGesture(g, null, null)) Log.w(ArgusApp.TAG, "gesture refused")
    }

    /* ── Keys and text ──────────────────────────────────────────────────── */

    fun deviceKey(name: String) {
        when (name) {
            "back" -> performGlobalAction(GLOBAL_ACTION_BACK)
            "home" -> performGlobalAction(GLOBAL_ACTION_HOME)
            "recents" -> performGlobalAction(GLOBAL_ACTION_RECENTS)
            "notifications" -> performGlobalAction(GLOBAL_ACTION_NOTIFICATIONS)
            "power" -> performGlobalAction(GLOBAL_ACTION_POWER_DIALOG)
            "lock" -> if (Build.VERSION.SDK_INT >= 28) performGlobalAction(GLOBAL_ACTION_LOCK_SCREEN)
            "volume_up" -> volume(AudioManager.ADJUST_RAISE)
            "volume_down" -> volume(AudioManager.ADJUST_LOWER)
            else -> Log.w(ArgusApp.TAG, "unknown device key $name")
        }
    }

    private fun volume(direction: Int) {
        val am = getSystemService(Context.AUDIO_SERVICE) as AudioManager
        am.adjustSuggestedStreamVolume(direction, AudioManager.USE_DEFAULT_STREAM_TYPE, AudioManager.FLAG_SHOW_UI)
    }

    /** A key by the browser's KeyboardEvent.code, on the field with focus. */
    fun key(code: String) {
        when (code) {
            "Escape" -> performGlobalAction(GLOBAL_ACTION_BACK)
            "Enter", "NumpadEnter" -> focused()?.let { n ->
                if (Build.VERSION.SDK_INT >= 30) {
                    n.performAction(AccessibilityNodeInfo.AccessibilityAction.ACTION_IME_ENTER.id)
                } else {
                    text("\n")
                }
            }
            "Backspace" -> focused()?.let { n ->
                val cur = n.text?.toString().orEmpty()
                if (cur.isNotEmpty()) setText(n, cur.dropLast(1))
            }
            "Space" -> text(" ")
            "Tab" -> Unit
            else -> {
                // KeyA..KeyZ and Digit0..Digit9 as their plain character; the
                // console's text field is the way to type anything else.
                val ch = when {
                    code.length == 4 && code.startsWith("Key") -> code[3].lowercaseChar().toString()
                    code.length == 6 && code.startsWith("Digit") -> code[5].toString()
                    else -> return
                }
                text(ch)
            }
        }
    }

    /** Typed text, appended to the field that has focus. */
    fun text(s: String) {
        val n = focused() ?: run {
            Log.w(ArgusApp.TAG, "typed text with no field focused")
            return
        }
        setText(n, n.text?.toString().orEmpty() + s)
    }

    private fun setText(n: AccessibilityNodeInfo, s: String) {
        val args = Bundle()
        args.putCharSequence(AccessibilityNodeInfo.ACTION_ARGUMENT_SET_TEXT_CHARSEQUENCE, s)
        n.performAction(AccessibilityNodeInfo.ACTION_SET_TEXT, args)
    }

    private fun focused(): AccessibilityNodeInfo? {
        val root = rootInActiveWindow ?: return null
        val n = root.findFocus(AccessibilityNodeInfo.FOCUS_INPUT) ?: return null
        return if (n.isEditable) n else null
    }

    /* ── The banner ─────────────────────────────────────────────────────── */

    /** Shows who is on the device, or takes the banner down with null. */
    fun banner(text: String?) {
        val wm = getSystemService(Context.WINDOW_SERVICE) as WindowManager
        banner?.let {
            try {
                wm.removeView(it)
            } catch (_: Exception) {
            }
            banner = null
        }
        if (text == null) return
        val bar = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            setBackgroundColor(Color.parseColor("#C62828"))
            setPadding(32, 16, 16, 16)
            gravity = Gravity.CENTER_VERTICAL
        }
        bar.addView(
            TextView(this).apply {
                this.text = text
                setTextColor(Color.WHITE)
                textSize = 14f
            },
            LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f),
        )
        bar.addView(
            Button(this).apply {
                this.text = getString(R.string.banner_stop)
                setOnClickListener { ArgusApp.instance.agent.endSessions() }
            },
        )
        val lp = WindowManager.LayoutParams(
            WindowManager.LayoutParams.MATCH_PARENT,
            WindowManager.LayoutParams.WRAP_CONTENT,
            WindowManager.LayoutParams.TYPE_ACCESSIBILITY_OVERLAY,
            WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE or WindowManager.LayoutParams.FLAG_LAYOUT_IN_SCREEN,
            PixelFormat.TRANSLUCENT,
        )
        lp.gravity = Gravity.TOP
        try {
            wm.addView(bar, lp)
            banner = bar
        } catch (e: Exception) {
            Log.w(ArgusApp.TAG, "banner not shown: ${e.message}")
        }
    }

    companion object {
        const val TAP_SLOP = 12f
        const val TAP_MS = 60L
        const val LONG_PRESS_MS = 400L
        const val MIN_SWIPE_MS = 80L
        const val MAX_SWIPE_MS = 1500L
        const val SCROLL_PX = 400f

        @Volatile
        var instance: InputService? = null
            private set
    }
}
