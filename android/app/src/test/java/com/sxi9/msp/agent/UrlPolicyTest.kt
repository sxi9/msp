package com.sxi9.msp.agent

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class UrlPolicyTest {
    @Test
    fun httpsAlwaysAllowed() {
        assertTrue(UrlPolicy.allowed("https://rmm.example.com", false))
        assertTrue(UrlPolicy.allowed("https://rmm.example.com:8443/", true))
    }

    @Test
    fun releaseRefusesCleartextExceptLoopback() {
        assertFalse(UrlPolicy.allowed("http://rmm.example.com", false))
        assertFalse(UrlPolicy.allowed("http://10.0.0.5:8080", false))
        assertFalse(UrlPolicy.allowed("http://127.0.0.1.evil.example", false))
        assertTrue(UrlPolicy.allowed("http://localhost:8080", false))
        assertTrue(UrlPolicy.allowed("http://127.0.0.1:8080", false))
        assertTrue(UrlPolicy.allowed("http://[::1]:8080", false))
        assertTrue(UrlPolicy.allowed("http://10.0.2.2:8080", false))
    }

    @Test
    fun debugBuildAllowsLabHttp() {
        assertTrue(UrlPolicy.allowed("http://10.0.0.5:8080", true))
    }

    @Test
    fun otherSchemesRefused() {
        assertFalse(UrlPolicy.allowed("ftp://x", false))
        assertFalse(UrlPolicy.allowed("rmm.example.com", true))
    }

    @Test
    fun hostParsing() {
        assertEquals("rmm.example.com", UrlPolicy.host("https://user:pw@RMM.example.com:8443/path?q#f"))
        assertEquals("::1", UrlPolicy.host("http://[::1]:8080/"))
        assertEquals("", UrlPolicy.host("nonsense"))
    }
}
