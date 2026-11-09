package com.gsoultan.argus.helper

/**
 * Who the helper serves: the one app that started it, by uid, and the file
 * that app installed it from. No Android in it, so it is tested on the JVM.
 *
 * The helper runs as shell and listens on an abstract local socket, which
 * any process on the device may connect to: the socket itself checks
 * nothing. Without this, an app installed later could connect and receive
 * the mirrored screen and inject input with shell's rights.
 */
class Access private constructor(val uid: Int, val dex: String) {
    /** Whether a peer with this uid may connect. */
    fun admits(peerUid: Int): Boolean = peerUid == uid

    companion object {
        /** App uids start here; anything below is the system or shell. */
        const val FIRST_APP_UID = 10_000

        /**
         * Reads `<app uid> <helper dex path>`. Null when either is missing
         * or not what the app passes: a uid below the app range (root,
         * system, shell itself), or a path that is not absolute.
         */
        fun parse(args: Array<String>): Access? {
            if (args.size < 2) return null
            val uid = args[0].toIntOrNull() ?: return null
            if (uid < FIRST_APP_UID) return null
            val dex = args[1]
            if (!dex.startsWith("/")) return null
            return Access(uid, dex)
        }
    }
}
