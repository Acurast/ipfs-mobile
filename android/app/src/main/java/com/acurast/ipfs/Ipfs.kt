package com.acurast.ipfs

import android.content.Context
import ffi.Ffi
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.Closeable
import java.io.File
import java.io.IOException
import kotlin.time.Duration
import kotlin.time.Duration.Companion.seconds
import ffi.Client as FfiClient

/**
 * A reusable IPFS client.
 *
 * The underlying node is started on the first download and reused by later ones,
 * so bootstrap peers are dialled once rather than on every fetch.
 *
 * Its lifetime is yours to choose:
 *
 *  - leave [idleTimeout] at its default and the node shuts itself down once it
 *    has been unused for that long, restarting on the next download. Nothing
 *    else is required, though [close] is still honoured.
 *  - pass `null` for [idleTimeout] to keep the node up until you [close] it,
 *    which suits a long lived client that fetches often.
 *
 * Instances are safe to share between coroutines.
 */
public class Ipfs(
    private val bootstrapNodes: List<String> = emptyList(),
    private val port: Int = PORT,
    private val idleTimeout: Duration? = IDLE_TIMEOUT,
) : Closeable {

    private val lock = Any()
    private var client: FfiClient? = null
    private var closed: Boolean = false

    public suspend fun get(cid: String, context: Context, sizeLimit: Long? = null, timeout: Duration? = null): File =
        get(cid, File(context.ipfsDataDir, cid), sizeLimit, timeout)

    public suspend fun get(cid: String, output: File, sizeLimit: Long? = null, timeout: Duration? = null): File = withContext(Dispatchers.IO) {
        try {
            client().get(
                cid,
                output.absolutePath,
                sizeLimit ?: NO_SIZE_LIMIT,
                timeout?.inWholeMilliseconds ?: NO_TIMEOUT,
            )

            output
        } catch (e: Throwable) {
            when {
                e.message?.startsWith("size limit exceeded") == true -> throw SizeLimitExceededException(e.message, e.cause)
                else -> throw IOException(e.message, e.cause)
            }
        }
    }

    /**
     * Shuts the node down and retires this client. Safe to call more than once,
     * but any later [get] fails.
     */
    override fun close() {
        val running = synchronized(lock) {
            if (closed) return

            closed = true
            client.also { client = null }
        }

        running?.close()
    }

    private fun client(): FfiClient = synchronized(lock) {
        if (closed) throw IOException("IPFS client is closed")

        // Cheap: this only validates the peer list, it does not touch the
        // network. The node itself comes up on the first download.
        client ?: Ffi.newClient(
            bootstrapNodes.joinToString(DELIMITER_LIST_STRING),
            port,
            idleTimeout?.inWholeMilliseconds ?: NO_TIMEOUT,
        ).also { client = it }
    }

    private val Context.ipfsDir: File
        get() = File(dataDir, DIR_IPFS).apply {
            if (!exists()) mkdir()
        }

    private fun Context.ipfsDir(child: String): File =
        File(ipfsDir, child).apply {
            if (!exists()) mkdirs()
        }

    private val Context.ipfsDataDir: File
        get() = ipfsDir(DIR_DATA)

    public companion object {
        private const val PORT = 0

        /**
         * Long enough that a burst of downloads reuses one set of connections,
         * short enough that an idle app is not left holding sockets open.
         */
        private val IDLE_TIMEOUT: Duration = 30.seconds

        /** The FFI encodes "no limit" and "no timeout" as a negative value. */
        private const val NO_SIZE_LIMIT = -1L
        private const val NO_TIMEOUT = -1L

        private const val DIR_IPFS = "ipfs"
        private const val DIR_DATA = "data"

        private const val DELIMITER_LIST_STRING = ";"
    }
}
