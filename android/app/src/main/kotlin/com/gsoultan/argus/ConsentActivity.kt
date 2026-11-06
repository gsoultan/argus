package com.gsoultan.argus

import android.app.Activity
import android.content.Context
import android.content.Intent
import android.media.projection.MediaProjectionManager
import android.os.Bundle
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity

/**
 * The question whoever holds the device answers before a session opens:
 * who, and whether they will watch or control. Accept goes on to Android's
 * own screen-capture prompt when the capture is not already running; only
 * both together are a yes.
 */
class ConsentActivity : AppCompatActivity() {
    private val app get() = application as ArgusApp
    private var answered = false
    private var id = 0L

    private val projection = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { res ->
        answered = true
        if (res.resultCode == Activity.RESULT_OK && res.data != null) {
            app.bridge.answer(id, true, res.data)
        } else {
            app.bridge.answer(id, false, null)
        }
        finish()
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        current = this
        id = intent.getLongExtra(EXTRA_ID, 0L)
        val person = intent.getStringExtra(EXTRA_PERSON).orEmpty().ifBlank { "Someone" }
        val control = intent.getStringExtra(EXTRA_PRINCIPAL) == "control"
        val needsProjection = intent.getBooleanExtra(EXTRA_NEEDS_PROJECTION, true)

        AlertDialog.Builder(this)
            .setTitle(R.string.consent_title)
            .setMessage(
                getString(if (control) R.string.consent_control else R.string.consent_view, person) +
                    "\n\n" + getString(R.string.consent_detail),
            )
            .setCancelable(false)
            .setPositiveButton(R.string.consent_accept) { _, _ ->
                if (needsProjection) {
                    val mpm = getSystemService(Context.MEDIA_PROJECTION_SERVICE) as MediaProjectionManager
                    projection.launch(mpm.createScreenCaptureIntent())
                } else {
                    answered = true
                    app.bridge.answer(id, true, null)
                    finish()
                }
            }
            .setNegativeButton(R.string.consent_decline) { _, _ ->
                answered = true
                app.bridge.answer(id, false, null)
                finish()
            }
            .show()
    }

    override fun onDestroy() {
        if (!answered) app.bridge.answer(id, false, null)
        if (current === this) current = null
        super.onDestroy()
    }

    companion object {
        const val EXTRA_ID = "id"
        const val EXTRA_PERSON = "person"
        const val EXTRA_PRINCIPAL = "principal"
        const val EXTRA_NEEDS_PROJECTION = "needsProjection"

        @Volatile
        private var current: ConsentActivity? = null

        /** The agent gave up waiting for question id: take it away. */
        fun dismiss(id: Long) {
            current?.let { if (it.id == id) it.finish() }
        }
    }
}
