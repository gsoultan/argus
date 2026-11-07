package com.gsoultan.argus

import android.content.pm.ApplicationInfo
import android.content.pm.PackageManager
import android.os.Build
import android.util.Log
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.TimeZone
import org.json.JSONArray
import org.json.JSONObject

/**
 * The device's installed apps -- its "what runs here", the way a server's is
 * its databases and listeners.
 *
 * Read from the package manager and handed to the agent, which reports it in
 * the device's facts (internal/control/android_apps.go) for the console's
 * Apps tab. Each app is its package (the stable id), the label the launcher
 * shows, its version, and where it came from: shipped with the device, from
 * the Play Store, or sideloaded.
 */
object InstalledApps {
    /** Lists the installed apps and hands them to the agent. */
    fun report(app: ArgusApp) {
        val root = JSONObject()
        root.put("checked_at", iso(Date()))
        root.put("apps", list(app.packageManager))
        try {
            app.agent.pushApps(root.toString())
        } catch (e: Exception) {
            Log.w(ArgusApp.TAG, "apps not pushed: ${e.message}")
        }
    }

    private fun list(pm: PackageManager): JSONArray {
        val arr = JSONArray()
        for (pkg in pm.getInstalledPackages(0)) {
            val info = pkg.applicationInfo ?: continue
            val o = JSONObject()
            o.put("package", pkg.packageName)
            o.put("label", pm.getApplicationLabel(info).toString())
            pkg.versionName?.let { o.put("version", it) }
            o.put("source", source(pm, info, pkg.packageName))
            arr.put(o)
        }
        return arr
    }

    private fun source(pm: PackageManager, info: ApplicationInfo, pkg: String): String {
        if (info.flags and ApplicationInfo.FLAG_SYSTEM != 0) return "preinstalled"
        val installer = try {
            if (Build.VERSION.SDK_INT >= 30) {
                pm.getInstallSourceInfo(pkg).installingPackageName
            } else {
                @Suppress("DEPRECATION")
                pm.getInstallerPackageName(pkg)
            }
        } catch (_: Exception) {
            null
        }
        // The Play Store is the installer of record for a Play app; anything
        // else a person put there by hand.
        return if (installer == "com.android.vending") "play" else "sideloaded"
    }

    private fun iso(d: Date): String {
        val f = SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss'Z'", Locale.US)
        f.timeZone = TimeZone.getTimeZone("UTC")
        return f.format(d)
    }
}
