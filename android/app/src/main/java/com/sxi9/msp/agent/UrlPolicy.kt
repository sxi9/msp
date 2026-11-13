package com.sxi9.msp.agent

/**
 * The agent transport policy (docs/PROTOCOL.md section 2), the same rule the desktop agent
 * applies in NewClient: https:// always; http:// only to a loopback host. Debug builds also
 * accept http:// to any host because their network security config permits cleartext for
 * lab servers; a release build never sends a token in the clear, whatever the user types.
 * Pure Kotlin so the JVM unit test can cover it.
 */
object UrlPolicy {
    fun allowed(server: String, debugBuild: Boolean): Boolean {
        val s = server.trim()
        if (s.startsWith("https://")) return true
        if (!s.startsWith("http://")) return false
        if (debugBuild) return true
        return isLoopback(host(s))
    }

    /** The host part of an http(s) URL: no scheme, userinfo, port, path; "" when malformed. */
    fun host(url: String): String {
        val afterScheme = url.substringAfter("://", "")
        if (afterScheme.isEmpty()) return ""
        val authority = afterScheme.takeWhile { it != '/' && it != '?' && it != '#' }.substringAfterLast('@')
        val h = if (authority.startsWith("[")) {
            authority.substringAfter('[').substringBefore(']')
        } else {
            authority.substringBefore(':')
        }
        return h.lowercase()
    }

    /** localhost, 127.0.0.0/8, ::1 and the emulator's host alias 10.0.2.2. */
    fun isLoopback(host: String): Boolean =
        host == "localhost" || host == "::1" || host == "10.0.2.2" ||
            (host.startsWith("127.") && host.split('.').size == 4)
}
