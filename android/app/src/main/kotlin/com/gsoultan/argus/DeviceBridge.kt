package com.gsoultan.argus

import android.content.Intent
import android.os.Handler
import android.os.Looper
import android.util.Log
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong
import java.util.concurrent.atomic.AtomicReference
import mobile.Device
import org.json.JSONObject

/**
 * What the agent asks of the app: mobile.Device, called from the agent's
 * own threads.
 *
 * Consent is the attended mode. The agent asks before a session opens; the
 * app puts the question in front of whoever holds the device
 * (ConsentActivity), then Android's own screen-capture prompt, and answers
 * yes only when both were accepted. Nothing is captured before that: the
 * capture starts with the first session and stops with the last.
 */
class DeviceBridge(private val app: ArgusApp) : Device {
    private val main = Handler(Looper.getMainLooper())

    /** The answer the activity is collecting, while one is open. */
    private val pending = AtomicReference<Pending?>()
    private val asked = AtomicLong()

    @Volatile
    private var capture: ScreenCapture? = null

    /**
     * The unattended engine, when the device is set up for it. Armed, it is
     * the screen and the finger instead of the capture and the accessibility
     * service, and a session opens without asking anyone.
     */
    @Volatile
    var unattended: UnattendedEngine? = null

    @Volatile
    private var person = ""

    @Volatile
    private var principal = ""

    private class Pending(val id: Long, val latch: CountDownLatch) {
        @Volatile
        var accepted = false

        @Volatile
        var projection: Intent? = null
    }

    fun who(): String = person.ifBlank { "Someone" }

    override fun consent(person: String, principal: String): Boolean {
        // A company device with nobody there: the helper is the screen, and
        // the Go agent has already decided this is unattended, so there is no
        // one to ask. The grant still named the person, recorded as the actor.
        if (unattended?.armed == true) return true
        val p = Pending(asked.incrementAndGet(), CountDownLatch(1))
        if (!pending.compareAndSet(null, p)) {
            // One question at a time; a second session waits for the first
            // to be answered, then is asked itself.
            pending.get()?.latch?.await(CONSENT_SECONDS, TimeUnit.SECONDS)
            if (!pending.compareAndSet(null, p)) return false
        }
        try {
            val question = Intent(app, ConsentActivity::class.java)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP)
                .putExtra(ConsentActivity.EXTRA_ID, p.id)
                .putExtra(ConsentActivity.EXTRA_PERSON, person)
                .putExtra(ConsentActivity.EXTRA_PRINCIPAL, principal)
                .putExtra(ConsentActivity.EXTRA_NEEDS_PROJECTION, capture == null)
            main.post {
                // Over whatever the holder is doing. An app with a bound
                // accessibility service may do this from the background;
                // the notification is the question for when it may not.
                try {
                    app.startActivity(question)
                } catch (e: Exception) {
                    Log.w(ArgusApp.TAG, "consent screen not shown: ${e.message}")
                }
                ConsentNotification.show(app, p.id, person, principal, question)
            }
            if (!p.latch.await(CONSENT_SECONDS, TimeUnit.SECONDS)) {
                main.post {
                    ConsentActivity.dismiss(p.id)
                    ConsentNotification.clear(app)
                }
                return false
            }
            main.post { ConsentNotification.clear(app) }
            if (!p.accepted) return false
            // Accepted on the device: recorded in the timeline as who said yes.
            app.activity.consentGiven(person)
            p.projection?.let { AgentService.capture(app, it) }
            return true
        } finally {
            pending.compareAndSet(p, null)
        }
    }

    /**
     * ConsentActivity's answer to question id; projection is the capture
     * prompt's result. An answer to a question already given up on is
     * dropped, never taken for the next one's.
     */
    fun answer(id: Long, accepted: Boolean, projection: Intent?) {
        val p = pending.get() ?: return
        if (p.id != id) return
        p.accepted = accepted
        p.projection = projection
        p.latch.countDown()
    }

    override fun sessionStarted(person: String, principal: String, open: Long) {
        this.person = person
        this.principal = principal
        main.post {
            AgentService.instance?.foreground(capture != null)
            InputService.instance?.banner(
                app.getString(
                    if (principal == "control") R.string.banner_control else R.string.banner_view,
                    person,
                ),
            )
        }
    }

    override fun sessionEnded(open: Long) {
        if (open > 0) return
        person = ""
        principal = ""
        val unattended = unattended?.armed == true
        main.post {
            InputService.instance?.banner(null)
            if (!unattended) AgentService.stopCapture(app)
        }
    }

    fun captureStarted(c: ScreenCapture?) {
        capture = c
    }

    override fun play(inputJSON: String) {
        unattended?.let { if (it.armed) { it.play(inputJSON); return } }
        val input = InputService.instance
            ?: throw IllegalStateException("the accessibility service is off on the device; it can be watched but not touched")
        val c = capture
        val j = JSONObject(inputJSON)
        val scale = c?.scale ?: 1f
        val x = (j.optInt("x") * scale)
        val y = (j.optInt("y") * scale)
        when (j.optString("type")) {
            "mouse" -> input.pointerMoved(x, y)
            "button" -> input.pointerButton(x, y, j.optBoolean("down"))
            "wheel" -> input.wheel(x, y, j.optInt("delta"))
            "key" -> if (j.optBoolean("down")) input.key(j.optString("code"))
            "device" -> input.deviceKey(j.optString("name"))
            "text" -> input.text(j.optString("text"))
            else -> Log.w(ArgusApp.TAG, "unknown input ${j.optString("type")}")
        }
    }

    companion object {
        const val CONSENT_SECONDS = 45L
    }
}
