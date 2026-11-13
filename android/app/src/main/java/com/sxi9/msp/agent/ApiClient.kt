package com.sxi9.msp.agent

import org.json.JSONObject
import java.io.IOException
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL

/** 401 from the server: the enrollment or device token is not accepted. */
class UnauthorizedException(message: String) : IOException(message)

/**
 * Minimal HttpURLConnection client for the three agent endpoints. Trusts the system store
 * only: production servers need a publicly trusted certificate, and self-signed CAs must be
 * installed as user certificates on the phone.
 */
object ApiClient {
    private const val TIMEOUT_MS = 15_000

    fun enroll(serverUrl: String, body: JSONObject): EnrollResult =
        Protocol.parseEnrollResponse(post(serverUrl, "/api/agent/v1/enroll", null, body))

    fun sync(serverUrl: String, token: String, body: JSONObject): SyncResponse =
        Protocol.parseSync(post(serverUrl, "/api/agent/v1/sync", token, body))

    fun postResult(serverUrl: String, token: String, commandId: Long, body: JSONObject) {
        post(serverUrl, "/api/agent/v1/commands/$commandId/result", token, body)
    }

    fun userAgent(arch: String): String = "msp-agent/$AGENT_VERSION (android/$arch)"

    /** POST JSON; returns the response body on 2xx, throws on anything else. */
    private fun post(serverUrl: String, path: String, token: String?, body: JSONObject): String {
        val url = URL(serverUrl.trimEnd('/') + path)
        val conn = url.openConnection() as HttpURLConnection
        try {
            conn.requestMethod = "POST"
            conn.connectTimeout = TIMEOUT_MS
            conn.readTimeout = TIMEOUT_MS
            conn.doOutput = true
            conn.useCaches = false
            conn.setRequestProperty("Content-Type", "application/json")
            conn.setRequestProperty("Accept", "application/json")
            conn.setRequestProperty("User-Agent", userAgent(Protocol.archOf(abi())))
            if (token != null) conn.setRequestProperty("Authorization", "Bearer $token")
            val payload = body.toString().toByteArray(Charsets.UTF_8)
            conn.setFixedLengthStreamingMode(payload.size)
            conn.outputStream.use { it.write(payload) }

            val code = conn.responseCode
            if (code in 200..299) {
                return conn.inputStream?.let { readAll(it) } ?: ""
            }
            val text = conn.errorStream?.let { readAll(it) } ?: ""
            val err = Protocol.parseError(text).ifEmpty { "http $code" }
            if (code == 401) throw UnauthorizedException(err)
            throw IOException(err)
        } finally {
            conn.disconnect()
        }
    }

    private fun readAll(s: InputStream): String = s.use { String(it.readBytes(), Charsets.UTF_8) }

    // Build.SUPPORTED_ABIS is read reflectively so this file stays loadable on the JVM.
    private fun abi(): String = try {
        val abis = Class.forName("android.os.Build").getField("SUPPORTED_ABIS").get(null) as? Array<*>
        abis?.firstOrNull()?.toString() ?: ""
    } catch (e: Throwable) {
        ""
    }
}
