package com.gsoultan.argus

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AlertDialog
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions
import mobile.Mobile
import kotlin.concurrent.thread

/**
 * The app's one screen: link this device to an Argus by scanning the code
 * the console shows (or pasting it), turn on the accessibility service, and
 * see whether the agent is connected.
 */
class MainActivity : AppCompatActivity() {
    private val app get() = application as ArgusApp
    private val handler = Handler(Looper.getMainLooper())

    private val scanner = registerForActivityResult(ScanContract()) { result ->
        result.contents?.let { enrol(it) }
    }
    private val notifications = registerForActivityResult(ActivityResultContracts.RequestPermission()) { }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        findViewById<Button>(R.id.scan).setOnClickListener {
            scanner.launch(
                ScanOptions()
                    .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                    .setPrompt(getString(R.string.scan))
                    .setBeepEnabled(false)
                    .setOrientationLocked(true),
            )
        }
        findViewById<Button>(R.id.link).setOnClickListener {
            val code = findViewById<EditText>(R.id.code).text.toString()
            if (code.isNotBlank()) enrol(code)
        }
        findViewById<Button>(R.id.enableInput).setOnClickListener {
            startActivity(Intent(Settings.ACTION_ACCESSIBILITY_SETTINGS))
        }
        findViewById<Button>(R.id.allowInstall).setOnClickListener {
            startActivity(Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:$packageName")))
        }
        findViewById<Button>(R.id.forget).setOnClickListener {
            AgentService.stop(this)
            thread { Mobile.forget(app.stateDir); handler.post { refresh() } }
        }
        findViewById<Button>(R.id.unattended).setOnClickListener {
            if (app.unattended.armed) {
                thread { app.unattended.disarm(); handler.post { refresh() } }
            } else {
                unattendedDialog()
            }
        }

        if (Build.VERSION.SDK_INT >= 33 &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            notifications.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    override fun onResume() {
        super.onResume()
        refresh()
        handler.postDelayed(ticker, 2000)
    }

    override fun onPause() {
        super.onPause()
        handler.removeCallbacks(ticker)
    }

    private val ticker = object : Runnable {
        override fun run() {
            refresh()
            handler.postDelayed(this, 2000)
        }
    }

    /** Links the device, then starts the agent: one scan is the whole setup. */
    private fun enrol(code: String) {
        thread {
            try {
                val c = Mobile.parseEnrolCode(code)
                val name = c.name.ifBlank { "${Build.MANUFACTURER}-${Build.MODEL}".lowercase().replace(Regex("[^a-z0-9-]+"), "-") }
                Mobile.enroll(c.getURL(), c.token, c.pin, name, app.stateDir)
                handler.post {
                    Toast.makeText(this, R.string.enrolled_ok, Toast.LENGTH_LONG).show()
                    findViewById<EditText>(R.id.code).setText("")
                    AgentService.start(this)
                    refresh()
                }
            } catch (e: Exception) {
                handler.post { Toast.makeText(this, e.message ?: e.toString(), Toast.LENGTH_LONG).show() }
            }
        }
    }

    private fun refresh() {
        val status = findViewById<TextView>(R.id.status)
        val connection = findViewById<TextView>(R.id.connection)
        val st = try { Mobile.enrolled(app.stateDir) } catch (e: Exception) { null }
        if (st != null && st.enrolled) {
            status.text = getString(R.string.status_enrolled, st.getControlURL(), st.hostname)
            if (!app.agent.running()) AgentService.start(this)
            connection.text = when {
                app.agent.sessions() > 0 -> getString(R.string.notif_session, app.bridge.who())
                app.agent.connected() -> getString(R.string.notif_connected)
                else -> getString(R.string.notif_connecting)
            }
        } else {
            status.text = getString(R.string.status_not_enrolled)
            connection.text = ""
        }
        findViewById<Button>(R.id.forget).isEnabled = st?.enrolled == true
        findViewById<TextView>(R.id.inputState).text =
            getString(if (InputService.instance != null) R.string.input_on else R.string.input_off)
        findViewById<Button>(R.id.enableInput).isEnabled = InputService.instance == null
        val armed = app.unattended.armed
        findViewById<TextView>(R.id.unattendedState).text =
            getString(if (armed) R.string.unattended_on else R.string.unattended_off)
        findViewById<Button>(R.id.unattended).text =
            getString(if (armed) R.string.unattended_disable else R.string.unattended_setup)
        findViewById<Button>(R.id.unattended).isEnabled = st?.enrolled == true
        val canInstall = packageManager.canRequestPackageInstalls()
        app.agent.setCanInstallApps(canInstall)
        findViewById<TextView>(R.id.installState).text =
            getString(if (canInstall) R.string.install_on else R.string.install_off)
        findViewById<Button>(R.id.allowInstall).isEnabled = !canInstall
        findViewById<TextView>(R.id.log).text = app.log.takeLast(12).joinToString("\n")
    }

    /**
     * Sets up unattended control: the person turns on Wireless debugging in
     * Developer options and enters the pairing address and code it shows. The
     * app pairs, keeps the key for after a reboot, and arms the engine.
     */
    private fun unattendedDialog() {
        val host = EditText(this).apply { hint = getString(R.string.pair_host); setSingleLine() }
        val code = EditText(this).apply { hint = getString(R.string.pair_code); setSingleLine() }
        val explain = TextView(this).apply { text = getString(R.string.unattended_explain); setPadding(0, 0, 0, 24) }
        val box = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(48, 24, 48, 0)
            addView(explain); addView(host); addView(code)
        }
        AlertDialog.Builder(this)
            .setTitle(R.string.unattended_title)
            .setView(box)
            .setPositiveButton(R.string.pair) { _, _ ->
                val hp = host.text.toString().trim().split(":")
                val h = hp.getOrNull(0).orEmpty()
                val port = hp.getOrNull(1)?.toIntOrNull() ?: 0
                val c = code.text.toString().trim()
                if (h.isBlank() || port == 0 || c.isBlank()) {
                    Toast.makeText(this, R.string.unattended_explain, Toast.LENGTH_LONG).show()
                    return@setPositiveButton
                }
                thread {
                    val err = app.unattended.setUp(h, port, c)
                    handler.post {
                        if (err != null) Toast.makeText(this, err, Toast.LENGTH_LONG).show()
                        refresh()
                    }
                }
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }
}
