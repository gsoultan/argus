package com.gsoultan.argus

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import android.os.Build
import android.provider.Settings
import android.util.Log
import java.io.File
import java.util.concurrent.CopyOnWriteArrayList
import mobile.Agent
import mobile.Logger
import mobile.Mobile

/**
 * One agent per process, made here so the service, the activities and the
 * accessibility service all speak to the same one.
 *
 * What the agent keeps -- the enrolment, the grant key pin, recordings not
 * yet shipped -- lives in the app's private files directory, which no other
 * app reads.
 */
class ArgusApp : Application() {
    lateinit var agent: Agent
        private set
    lateinit var bridge: DeviceBridge
        private set

    val stateDir: String get() = File(filesDir, "agent").absolutePath

    /** The last lines of the agent's log, for the main screen. */
    val log = CopyOnWriteArrayList<String>()

    override fun onCreate() {
        super.onCreate()
        instance = this
        File(stateDir).mkdirs()
        bridge = DeviceBridge(this)
        agent = Mobile.newAgent(
            stateDir, bridge, AppLogger(this),
            "Android ${Build.VERSION.RELEASE}",
            "${Build.MANUFACTURER} ${Build.MODEL}".trim(),
            Settings.Secure.getString(contentResolver, Settings.Secure.ANDROID_ID) ?: "",
        )
        channels()
    }

    private fun channels() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL_STATUS, getString(R.string.channel_status), NotificationManager.IMPORTANCE_LOW),
        )
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL_SESSION, getString(R.string.channel_session), NotificationManager.IMPORTANCE_HIGH),
        )
    }

    fun note(line: String) {
        log.add(line)
        while (log.size > 60) log.removeAt(0)
    }

    companion object {
        const val TAG = "argus"
        const val CHANNEL_STATUS = "argus.status"
        const val CHANNEL_SESSION = "argus.session"

        lateinit var instance: ArgusApp
            private set
    }
}

/** The agent's log lines, in logcat and on the main screen. */
private class AppLogger(private val app: ArgusApp) : Logger {
    override fun log(level: String, msg: String) {
        when (level) {
            "error" -> Log.e(ArgusApp.TAG, msg)
            "warn" -> Log.w(ArgusApp.TAG, msg)
            "debug" -> Log.d(ArgusApp.TAG, msg)
            else -> Log.i(ArgusApp.TAG, msg)
        }
        if (level != "debug") app.note("$level $msg")
    }
}
