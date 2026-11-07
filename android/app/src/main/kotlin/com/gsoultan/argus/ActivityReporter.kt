package com.gsoultan.argus

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.util.Log
import android.view.accessibility.AccessibilityEvent
import org.json.JSONObject

/**
 * Turns what the device is doing into the session's activity timeline.
 *
 * The console records the screen as pixels; this records the context beside
 * them -- which app came to the front, when the screen locked or unlocked,
 * when an app hid its window, when text was typed, when the holder accepted
 * or ended the session -- so an auditor replaying a session reads what
 * happened, not only what it looked like. The agent chains each event into
 * the recording next to the frames (internal/agent/android.go).
 *
 * A password is never in the recording. When the field that was typed into
 * is a password field, the timeline says so and carries no text.
 */
class ActivityReporter(private val app: ArgusApp) {
    private var lastPackage = ""

    @Volatile
    private var locked = false

    @Volatile
    private var dark = false

    /** The agent chains this into the recording, if a session is open. */
    private fun push(o: JSONObject) {
        try {
            app.agent.pushActivity(o.toString())
        } catch (e: Exception) {
            Log.w(ArgusApp.TAG, "activity not pushed: ${e.message}")
        }
    }

    /** An accessibility event: the app in front, or text typed. */
    fun onEvent(event: AccessibilityEvent) {
        when (event.eventType) {
            AccessibilityEvent.TYPE_WINDOW_STATE_CHANGED -> {
                val pkg = event.packageName?.toString().orEmpty()
                // Argus's own windows (the consent screen, the banner) are not
                // the device's activity; the holder's apps are.
                if (pkg.isBlank() || pkg == app.packageName) return
                if (pkg == lastPackage) return
                lastPackage = pkg
                val title = event.text?.joinToString(" ")?.takeIf { it.isNotBlank() }.orEmpty()
                val o = JSONObject().put("kind", "app").put("package", pkg)
                if (title.isNotEmpty()) o.put("title", title)
                push(o)
            }
            AccessibilityEvent.TYPE_VIEW_TEXT_CHANGED -> {
                // isPassword marks the field the characters fell into. Its
                // text is never carried; the timeline says only that a
                // password was typed.
                val o = JSONObject().put("kind", "typed")
                if (event.isPassword) {
                    o.put("password", true)
                } else {
                    val text = event.text?.joinToString("")?.takeIf { it.isNotBlank() } ?: return
                    o.put("text", text)
                }
                push(o)
            }
        }
    }

    /** Whoever holds the device accepted the session. */
    fun consentGiven(by: String) {
        push(JSONObject().put("kind", "consent").put("by", by.ifBlank { "device" }))
    }

    /** The session was ended on the device, from the banner's Stop. */
    fun endedOnDevice(by: String) {
        push(JSONObject().put("kind", "ended").put("by", by.ifBlank { "device" }))
    }

    /**
     * The capture went dark or came back. A FLAG_SECURE window blanks the
     * mirror, so a run of black frames while the screen is on is secure
     * content hidden. A locked screen is black too, and is reported as a lock
     * rather than twice, so darkness while locked is ignored here.
     */
    fun frameDark(isDark: Boolean) {
        if (isDark == dark) return
        dark = isDark
        if (locked) return
        push(JSONObject().put("kind", "secure").put("hidden", isDark))
    }

    /** Screen on/off/user-present, as lock and unlock. */
    private val screen = object : BroadcastReceiver() {
        override fun onReceive(context: Context?, intent: Intent?) {
            when (intent?.action) {
                Intent.ACTION_SCREEN_OFF -> {
                    locked = true
                    push(JSONObject().put("kind", "lock"))
                }
                Intent.ACTION_USER_PRESENT -> {
                    locked = false
                    push(JSONObject().put("kind", "unlock"))
                }
            }
        }
    }

    fun register() {
        val filter = IntentFilter().apply {
            addAction(Intent.ACTION_SCREEN_OFF)
            addAction(Intent.ACTION_USER_PRESENT)
        }
        app.registerReceiver(screen, filter)
    }

    fun unregister() {
        try {
            app.unregisterReceiver(screen)
        } catch (_: Exception) {
        }
    }
}
