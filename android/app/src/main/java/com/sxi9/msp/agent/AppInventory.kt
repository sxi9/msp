package com.sxi9.msp.agent

import android.content.Context
import android.content.pm.ApplicationInfo
import android.content.pm.PackageInfo
import android.content.pm.PackageManager
import android.os.Build
import android.util.Log

/**
 * Reads every installed package through PackageManager. On API 30+ the list is complete only
 * because the manifest declares QUERY_ALL_PACKAGES (a device-management agent must see the
 * whole inventory, not a <queries> allow-list). Apps.toJson shapes the result for facts.apps.
 */
object AppInventory {
    fun collect(ctx: Context): List<InstalledApp> {
        val pm = ctx.packageManager
        val packages: List<PackageInfo> = try {
            if (Build.VERSION.SDK_INT >= 33) {
                pm.getInstalledPackages(PackageManager.PackageInfoFlags.of(0L))
            } else {
                @Suppress("DEPRECATION")
                pm.getInstalledPackages(0)
            }
        } catch (e: Exception) {
            Log.w("msp-agent", "installed packages unavailable: ${e.message}")
            return emptyList()
        }
        return packages.map { p ->
            val ai: ApplicationInfo? = p.applicationInfo
            InstalledApp(
                packageName = p.packageName ?: "",
                label = try { ai?.loadLabel(pm)?.toString() } catch (e: Exception) { null },
                version = p.versionName,
                system = ai != null && (ai.flags and ApplicationInfo.FLAG_SYSTEM) != 0,
            )
        }
    }
}
