package com.gsoultan.argus

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import androidx.core.app.NotificationCompat
import mobile.Removed

/**
 * The app's part of leaving Argus, once the Go agent has revoked and deleted
 * the device's link (mobile.Agent.leave), whether from the Remove button or
 * because an administrator removed the device in the console.
 *
 * What the app set up is taken down here: the unattended engine is disarmed,
 * wireless debugging turned back off and its pairing deleted, so nothing
 * re-arms at the next boot; the accessibility service switches itself off;
 * the foreground service stops. Android lets no app uninstall itself or take
 * back "Install unknown apps" for itself, so the holder is told, with a
 * button that opens Android's own uninstall question.
 */
class Removal(private val app: ArgusApp) : Removed {
    override fun removed(byConsole: Boolean, summary: String) {
        try {
            app.unattended.forget()
        } catch (e: Exception) {
            app.note("leaving: unattended engine: ${e.message}")
        }
        // API 24+: an accessibility service may turn itself off; the holder
        // would otherwise have to find it in Settings.
        InputService.instance?.disableSelf()
        try {
            AgentService.stop(app)
        } catch (e: Exception) {
            app.note("leaving: the service did not stop: ${e.message}")
        }
        app.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putString(KEY_SUMMARY, summary).apply()
        notify(app, summary)
        app.note(summary)
    }

    companion object {
        const val PREFS = "argus.removal"
        const val KEY_SUMMARY = "summary"
        private const val CHANNEL = "argus.removed"
        private const val ID = 3

        /** Android's own question: uninstall this app? */
        fun uninstallIntent(ctx: Context): Intent =
            Intent(Intent.ACTION_DELETE, Uri.parse("package:${ctx.packageName}"))
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

        /** What leaving said last, until the app is linked again. */
        fun lastSummary(ctx: Context): String? =
            ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString(KEY_SUMMARY, null)

        fun clear(ctx: Context) {
            ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().remove(KEY_SUMMARY).apply()
        }

        private fun notify(ctx: Context, summary: String) {
            val nm = ctx.getSystemService(NotificationManager::class.java)
            nm.createNotificationChannel(
                NotificationChannel(CHANNEL, ctx.getString(R.string.channel_removed), NotificationManager.IMPORTANCE_DEFAULT),
            )
            val text = summary + " " + ctx.getString(R.string.left_next)
            val open = PendingIntent.getActivity(
                ctx, 0, Intent(ctx, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE,
            )
            val uninstall = PendingIntent.getActivity(
                ctx, 1, uninstallIntent(ctx), PendingIntent.FLAG_IMMUTABLE,
            )
            val n = NotificationCompat.Builder(ctx, CHANNEL)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(ctx.getString(R.string.left_title))
                .setContentText(text)
                .setStyle(NotificationCompat.BigTextStyle().bigText(text))
                .setContentIntent(open)
                .addAction(0, ctx.getString(R.string.uninstall_app), uninstall)
                .setAutoCancel(true)
                .build()
            nm.notify(ID, n)
        }
    }
}
