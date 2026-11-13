package com.sxi9.msp.agent

import android.Manifest
import android.app.Activity
import android.app.admin.DevicePolicyManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.widget.Button
import android.widget.EditText
import android.widget.TextView
import android.widget.Toast

/** Enrollment screen: server URL + token, service controls, background/admin grants. */
class MainActivity : Activity() {
    private lateinit var prefs: Prefs
    private lateinit var serverUrl: EditText
    private lateinit var token: EditText
    private lateinit var status: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)
        prefs = Prefs(this)
        serverUrl = findViewById(R.id.server_url)
        token = findViewById(R.id.token)
        status = findViewById(R.id.status)
        if (prefs.serverUrl.isNotEmpty()) serverUrl.setText(prefs.serverUrl)

        findViewById<Button>(R.id.btn_enroll).setOnClickListener { enroll() }
        findViewById<Button>(R.id.btn_start).setOnClickListener {
            if (prefs.enrolled) AgentService.start(this) else toast("Enroll first")
            refresh()
        }
        findViewById<Button>(R.id.btn_stop).setOnClickListener {
            AgentService.stop(this)
            refresh()
        }
        findViewById<Button>(R.id.btn_battery).setOnClickListener { requestBatteryExemption() }
        findViewById<Button>(R.id.btn_admin).setOnClickListener { requestDeviceAdmin() }

        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
    }

    override fun onResume() {
        super.onResume()
        refresh()
    }

    private fun enroll() {
        val server = serverUrl.text.toString().trim().trimEnd('/')
        val et = token.text.toString().trim()
        if (!(server.startsWith("https://") || server.startsWith("http://")) || server.length < 10) {
            toast("Enter the server URL (https://...)")
            return
        }
        if (!et.startsWith("et_")) {
            toast("Enter the enrollment token (et_...)")
            return
        }
        status.text = "Enrolling..."
        Thread {
            try {
                val body = Protocol.buildEnrollJson(
                    et, Metrics.deviceUid(this), Metrics.hostname(this), Metrics.arch(),
                    Metrics.osVersion(), Metrics.facts(this)
                )
                val r = ApiClient.enroll(server, body)
                prefs.saveEnrollment(server, r)
                runOnUiThread {
                    token.setText("")
                    AgentService.start(this)
                    toast("Enrolled as device ${r.deviceId}")
                    refresh()
                }
            } catch (e: Exception) {
                runOnUiThread {
                    status.text = "Enrollment failed: ${e.message}"
                }
            }
        }.start()
    }

    private fun requestBatteryExemption() {
        val i = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
        i.data = Uri.parse("package:$packageName")
        try {
            startActivity(i)
        } catch (e: Exception) {
            startActivity(Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS))
        }
    }

    private fun requestDeviceAdmin() {
        val i = Intent(DevicePolicyManager.ACTION_ADD_DEVICE_ADMIN)
        i.putExtra(DevicePolicyManager.EXTRA_DEVICE_ADMIN, AdminReceiver.component(this))
        i.putExtra(DevicePolicyManager.EXTRA_ADD_EXPLANATION, getString(R.string.admin_explanation))
        startActivity(i)
    }

    private fun refresh() {
        if (!prefs.enrolled) {
            status.text = getString(R.string.status_idle)
            return
        }
        val pm = getSystemService(Context.POWER_SERVICE) as PowerManager
        val lines = listOf(
            "Server:        ${prefs.serverUrl}",
            "Device id:     ${prefs.deviceId}",
            "Interval:      ${prefs.intervalS} s",
            "Last sync:     ${prefs.lastSync.ifEmpty { "none yet" }}",
            "Background:    ${if (pm.isIgnoringBatteryOptimizations(packageName)) "allowed" else "restricted (tap Allow background)"}",
            "Device admin:  ${if (AdminReceiver.isActive(this)) "active (remote lock enabled)" else "not granted"}",
        )
        status.text = lines.joinToString("\n")
    }

    private fun toast(msg: String) = Toast.makeText(this, msg, Toast.LENGTH_SHORT).show()
}
