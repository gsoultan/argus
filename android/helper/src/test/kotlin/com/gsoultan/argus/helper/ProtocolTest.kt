package com.gsoultan.argus.helper

import java.io.ByteArrayInputStream
import java.io.ByteArrayOutputStream
import java.io.DataInputStream
import java.io.DataOutputStream
import kotlin.test.Test
import kotlin.test.assertContentEquals
import kotlin.test.assertEquals
import kotlin.test.assertFailsWith
import kotlin.test.assertNull

/**
 * The framing between the app and the helper, read back on the JVM so a
 * device is not needed to know it is right.
 */
class ProtocolTest {
    private fun roundTrip(write: (DataOutputStream) -> Unit): Protocol.Message {
        val buf = ByteArrayOutputStream()
        write(DataOutputStream(buf))
        val msg = Protocol.read(DataInputStream(ByteArrayInputStream(buf.toByteArray())))
        return msg!!
    }

    @Test
    fun inputIsAJsonLine() {
        val m = roundTrip { Protocol.writeInput(it, """{"type":"device","name":"home"}""") }
        assertEquals(Protocol.INPUT, m.type)
        assertEquals("""{"type":"device","name":"home"}""", m.text())
    }

    @Test
    fun sizeCarriesTwoInts() {
        val m = roundTrip { Protocol.writeSize(it, 1080, 2400) }
        assertEquals(Protocol.SIZE, m.type)
        assertEquals(1080, m.int(0))
        assertEquals(2400, m.int(4))
    }

    @Test
    fun frameCarriesItsHeaderThenRows() {
        val rows = byteArrayOf(1, 2, 3, 4, 5, 6, 7, 8)
        val m = roundTrip { Protocol.writeFrame(it, 2, 1, 8, rows, rows.size) }
        assertEquals(Protocol.FRAME, m.type)
        assertEquals(2, m.int(0))
        assertEquals(1, m.int(4))
        assertEquals(8, m.int(8))
        assertContentEquals(rows, m.body.copyOfRange(12, m.body.size))
    }

    @Test
    fun aCleanEndOfStreamIsNull() {
        assertNull(Protocol.read(DataInputStream(ByteArrayInputStream(ByteArray(0)))))
    }

    @Test
    fun anAbsurdLengthIsRefused() {
        val buf = ByteArrayOutputStream()
        val out = DataOutputStream(buf)
        out.writeByte(Protocol.FRAME)
        out.writeInt(Protocol.MAX_MESSAGE + 1)
        assertFailsWith<IllegalArgumentException> {
            Protocol.read(DataInputStream(ByteArrayInputStream(buf.toByteArray())))
        }
    }
}
