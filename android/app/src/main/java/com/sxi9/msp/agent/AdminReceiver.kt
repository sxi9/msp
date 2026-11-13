package com.sxi9.msp.agent

import android.app.admin.DeviceAdminReceiver
import android.app.admin.DevicePolicyManager
import android.content.ComponentName
import android.content.Context

/** Device admin with the force-lock policy only (res/xml/device_admin.xml). */
class AdminReceiver : DeviceAdminReceiver() {
    companion object {
        fun component(ctx: Context): ComponentName = ComponentName(ctx, AdminReceiver::class.java)

        fun isActive(ctx: Context): Boolean {
            val dpm = ctx.getSystemService(Context.DEVICE_POLICY_SERVICE) as DevicePolicyManager
            return dpm.isAdminActive(component(ctx))
        }
    }
}
