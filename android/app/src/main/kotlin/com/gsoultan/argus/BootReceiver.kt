package com.gsoultan.argus

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * After a reboot: start the agent again, and — on a device set up for the
 * unattended engine — re-arm it, since wireless debugging and the shell
 * helper do not survive a restart. This is why pairing is kept: a company
 * device comes back on its own, with nobody there to open the app.
 *
 * A device that was never set up for unattended just starts the agent, the
 * same as the person opening the app would.
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED &&
            intent.action != Intent.ACTION_LOCKED_BOOT_COMPLETED
        ) {
            return
        }
        val app = context.applicationContext as ArgusApp
        val enrolled = try {
            mobile.Mobile.enrolled(app.stateDir)?.enrolled == true
        } catch (_: Exception) {
            false
        }
        if (!enrolled) return
        AgentService.start(context)
    }
}
