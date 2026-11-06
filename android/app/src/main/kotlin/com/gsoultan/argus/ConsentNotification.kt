package com.gsoultan.argus

import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import androidx.core.app.NotificationCompat

/**
 * The session question as a notification, beside the screen that asks it:
 * for a device where Argus may not put a screen up from the background,
 * the notification is how the holder reaches the question. Opening it
 * opens the same ConsentActivity.
 */
object ConsentNotification {
    const val ID = 2

    fun show(ctx: Context, id: Long, person: String, principal: String, question: Intent) {
        val open = PendingIntent.getActivity(
            ctx, id.toInt(), question, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val text = ctx.getString(
            if (principal == "control") R.string.consent_control else R.string.consent_view,
            person.ifBlank { "Someone" },
        )
        val n = NotificationCompat.Builder(ctx, ArgusApp.CHANNEL_SESSION)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(ctx.getString(R.string.consent_title))
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setCategory(NotificationCompat.CATEGORY_CALL)
            .setAutoCancel(true)
            .setOngoing(true)
            .setContentIntent(open)
            .setFullScreenIntent(open, true)
            .setTimeoutAfter(DeviceBridge.CONSENT_SECONDS * 1000)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(ID, n)
    }

    fun clear(ctx: Context) {
        ctx.getSystemService(NotificationManager::class.java).cancel(ID)
    }
}
