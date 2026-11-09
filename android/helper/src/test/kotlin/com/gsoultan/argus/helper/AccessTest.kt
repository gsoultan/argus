package com.gsoultan.argus.helper

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

/**
 * The helper serves the app that started it and nobody else: the abstract
 * socket it listens on lets any process on the device connect.
 */
class AccessTest {
    @Test
    fun onlyTheAppThatStartedItIsServed() {
        val a = assertNotNull(Access.parse(arrayOf("10234", "/data/user/0/com.gsoultan.argus/files/argus-helper.dex")))
        assertEquals(10234, a.uid)
        assertTrue(a.admits(10234))
        assertFalse(a.admits(10235), "another app")
        assertFalse(a.admits(2000), "shell itself")
        assertFalse(a.admits(0), "root")
        assertFalse(a.admits(-1), "a peer whose uid could not be read")
    }

    @Test
    fun withoutTheAppsUidItDoesNotStart() {
        assertNull(Access.parse(arrayOf()), "no arguments, as an older app started it")
        assertNull(Access.parse(arrayOf("10234")), "no file to watch")
        assertNull(Access.parse(arrayOf("two", "/x.dex")))
        assertNull(Access.parse(arrayOf("2000", "/x.dex")), "shell's uid")
        assertNull(Access.parse(arrayOf("0", "/x.dex")), "root's uid")
        assertNull(Access.parse(arrayOf("10234", "x.dex")), "a relative path")
    }
}
