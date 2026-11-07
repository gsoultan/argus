package com.gsoultan.argus.helper

import java.io.DataInputStream
import java.io.DataOutputStream

/**
 * The wire between the app and the helper, over one local socket.
 *
 * The app sends input to play; the helper sends frames and the device's size.
 * Every message is a one-byte type then a big-endian length then the bytes,
 * so neither side has to guess where one ends. It is deliberately small: the
 * helper is shell-privileged code, so the less it parses the better.
 *
 * This file has no Android in it on purpose — it is the one part of the helper
 * that runs on the JVM in a test, so the framing is checked without a device.
 */
object Protocol {
    // App → helper.
    const val INPUT: Int = 1 // an AndroidInput JSON line, UTF-8
    const val STOP: Int = 2 // end: the helper exits

    // Helper → app.
    const val SIZE: Int = 16 // width,height as two big-endian int32
    const val FRAME: Int = 17 // width,height,stride int32 then RGBA rows

    const val MAX_MESSAGE: Int = 64 * 1024 * 1024

    fun writeInput(out: DataOutputStream, json: String) {
        val b = json.toByteArray(Charsets.UTF_8)
        frame(out, INPUT, b)
    }

    fun writeStop(out: DataOutputStream) {
        frame(out, STOP, ByteArray(0))
    }

    fun writeSize(out: DataOutputStream, width: Int, height: Int) {
        val b = ByteArray(8)
        putInt(b, 0, width)
        putInt(b, 4, height)
        frame(out, SIZE, b)
    }

    /** payload is width,height,stride then the RGBA rows. */
    fun writeFrame(out: DataOutputStream, width: Int, height: Int, stride: Int, rows: ByteArray, rowsLen: Int) {
        val head = ByteArray(12)
        putInt(head, 0, width)
        putInt(head, 4, height)
        putInt(head, 8, stride)
        out.writeByte(FRAME)
        out.writeInt(12 + rowsLen)
        out.write(head)
        out.write(rows, 0, rowsLen)
    }

    private fun frame(out: DataOutputStream, type: Int, body: ByteArray) {
        out.writeByte(type)
        out.writeInt(body.size)
        out.write(body)
    }

    /** One message, or null at a clean end of stream. */
    fun read(input: DataInputStream): Message? {
        val type = input.read()
        if (type < 0) return null
        val len = input.readInt()
        require(len in 0..MAX_MESSAGE) { "message length $len out of range" }
        val body = ByteArray(len)
        input.readFully(body)
        return Message(type, body)
    }

    class Message(val type: Int, val body: ByteArray) {
        fun text(): String = String(body, Charsets.UTF_8)

        fun int(at: Int): Int =
            (body[at].toInt() and 0xFF shl 24) or
                (body[at + 1].toInt() and 0xFF shl 16) or
                (body[at + 2].toInt() and 0xFF shl 8) or
                (body[at + 3].toInt() and 0xFF)
    }

    private fun putInt(b: ByteArray, at: Int, v: Int) {
        b[at] = (v ushr 24).toByte()
        b[at + 1] = (v ushr 16).toByte()
        b[at + 2] = (v ushr 8).toByte()
        b[at + 3] = v.toByte()
    }
}
