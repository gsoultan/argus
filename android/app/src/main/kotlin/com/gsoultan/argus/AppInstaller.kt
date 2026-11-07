package com.gsoultan.argus

import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.os.Build
import android.provider.Settings
import android.util.Log
import androidx.core.app.NotificationCompat
import java.io.File
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import mobile.Installer

/**
 * How the console's Apps tab changes apps on this device.
 *
 * The agent has already fetched an install's file and checked it is the
 * package, version and signer the console showed; this only asks Android.
 *
 * Attended (the default): Android's own installer asks whoever holds the
 * device -- "Do you want to install this app?" -- and nothing installs or
 * uninstalls without their yes. The question is put up as a notification
 * with a full-screen intent, the way the session question is, because an
 * app in the background may not open a screen itself. Nobody answering in
 * ten minutes is a failed run, and the session is abandoned.
 *
 * Unattended: the shell helper's ADB channel runs `pm`, silently; only it
 * can disable a preinstalled app for the device's user, which is never a
 * true removal and is undone with `cmd package install-existing`.
 *
 * Never touched from here, whatever the console asks: this app, the
 * launcher in use, the keyboard in use, Settings and System UI.
 */
class AppInstaller(private val app: ArgusApp) : Installer {
    override fun install(path: String, pkg: String) {
        guard(pkg)
        val file = File(path)
        if (app.unattended.armed) {
            app.unattended.shellWithInput("pm install -r -S ${file.length()}", file)
            done()
            return
        }
        if (!app.packageManager.canRequestPackageInstalls()) {
            throw Exception("Install unknown apps is not allowed for Argus on this device; open Argus to allow it")
        }
        val pi = app.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL)
        params.setAppPackageName(pkg)
        params.setSize(file.length())
        val session = pi.createSession(params)
        try {
            pi.openSession(session).use { s ->
                s.openWrite("base.apk", 0, file.length()).use { out ->
                    file.inputStream().use { it.copyTo(out) }
                    s.fsync(out)
                }
                waitFor("install", pkg) { sender -> s.commit(sender) }
            }
        } catch (e: Exception) {
            try {
                pi.abandonSession(session)
            } catch (_: Exception) {
            }
            throw e
        }
        done()
    }

    override fun uninstall(pkg: String) {
        guard(pkg)
        if (app.unattended.armed) {
            app.unattended.shell("pm uninstall $pkg")
        } else {
            waitFor("uninstall", pkg) { sender -> app.packageManager.packageInstaller.uninstall(pkg, sender) }
        }
        done()
    }

    override fun disable(pkg: String) {
        guard(pkg)
        if (!app.unattended.armed) throw Exception("disabling a preinstalled app needs the unattended engine")
        app.unattended.shell("pm uninstall -k --user 0 $pkg")
        done()
    }

    override fun restore(pkg: String) {
        if (!PACKAGE.matches(pkg)) throw Exception("not an application id")
        if (!app.unattended.armed) throw Exception("restoring a preinstalled app needs the unattended engine")
        app.unattended.shell("cmd package install-existing $pkg")
        done()
    }

    /** What is never changed from the console, checked on the device. */
    private fun guard(pkg: String) {
        if (!PACKAGE.matches(pkg)) throw Exception("not an application id")
        val pm = app.packageManager
        val home = pm.resolveActivity(Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_HOME), 0)
            ?.activityInfo?.packageName
        val ime = Settings.Secure.getString(app.contentResolver, Settings.Secure.DEFAULT_INPUT_METHOD)
            ?.substringBefore('/')
        when (pkg) {
            app.packageName -> throw Exception("this is the Argus app, how Argus reaches the device")
            home -> throw Exception("this is the launcher in use")
            ime -> throw Exception("this is the keyboard in use")
            "com.android.settings", "com.android.systemui" -> throw Exception("the device cannot be used without it")
        }
    }

    /** Reads the installed apps again, so the console sees the change. */
    private fun done() {
        InstalledApps.report(app)
    }

    /**
     * Runs start with an IntentSender that Android answers on, puts Android's
     * question in front of the person when it has one, and waits for the
     * outcome.
     */
    private fun waitFor(what: String, pkg: String, start: (android.content.IntentSender) -> Unit) {
        val op = next.incrementAndGet()
        val answers = LinkedBlockingQueue<Intent>()
        pending[op] = answers
        try {
            val flags = PendingIntent.FLAG_UPDATE_CURRENT or
                (if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0)
            val reply = PendingIntent.getBroadcast(
                app, op, Intent(app, AppInstallReceiver::class.java).putExtra(EXTRA_OP, op), flags,
            )
            start(reply.intentSender)
            val deadline = System.currentTimeMillis() + ANSWER_SECONDS * 1000
            while (true) {
                val left = deadline - System.currentTimeMillis()
                val answer = answers.poll(left.coerceAtLeast(0), TimeUnit.MILLISECONDS)
                    ?: throw Exception("nobody on the device answered Android's question within ${ANSWER_SECONDS / 60} minutes")
                when (val status = answer.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)) {
                    PackageInstaller.STATUS_PENDING_USER_ACTION -> ask(what, pkg, op, answer)
                    PackageInstaller.STATUS_SUCCESS -> return
                    PackageInstaller.STATUS_FAILURE_ABORTED ->
                        throw Exception("the person holding the device said no")
                    else -> throw Exception(
                        answer.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE)
                            ?: "Android refused it (status $status)",
                    )
                }
            }
        } finally {
            pending.remove(op)
            app.getSystemService(NotificationManager::class.java).cancel(NOTIFICATION_ID)
        }
    }

    /** Puts Android's own confirmation in front of whoever holds the device. */
    private fun ask(what: String, pkg: String, op: Int, answer: Intent) {
        @Suppress("DEPRECATION")
        val confirm = (if (Build.VERSION.SDK_INT >= 33) {
            answer.getParcelableExtra(Intent.EXTRA_INTENT, Intent::class.java)
        } else {
            answer.getParcelableExtra(Intent.EXTRA_INTENT)
        }) ?: throw Exception("Android asked for a confirmation it did not provide")
        confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        val open = PendingIntent.getActivity(app, op, confirm, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val text = app.getString(if (what == "install") R.string.app_install_ask else R.string.app_uninstall_ask, pkg)
        val n = NotificationCompat.Builder(app, ArgusApp.CHANNEL_SESSION)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(app.getString(R.string.app_change_title))
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setAutoCancel(true)
            .setContentIntent(open)
            .setFullScreenIntent(open, true)
            .setTimeoutAfter(ANSWER_SECONDS * 1000)
            .build()
        app.getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, n)
        try {
            app.startActivity(confirm)
        } catch (e: Exception) {
            Log.i(ArgusApp.TAG, "the install question waits in the notification: ${e.message}")
        }
    }

    companion object {
        const val EXTRA_OP = "com.gsoultan.argus.APP_OP"
        const val NOTIFICATION_ID = 3
        const val ANSWER_SECONDS = 600L
        private val PACKAGE = Regex("^[A-Za-z][A-Za-z0-9_]*(\\.[A-Za-z][A-Za-z0-9_]*)+$")
        private val next = AtomicInteger(1000)
        internal val pending = ConcurrentHashMap<Int, LinkedBlockingQueue<Intent>>()
    }
}

/** Where Android answers an install or uninstall the app started. */
class AppInstallReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val op = intent.getIntExtra(AppInstaller.EXTRA_OP, -1)
        AppInstaller.pending[op]?.offer(intent)
    }
}
