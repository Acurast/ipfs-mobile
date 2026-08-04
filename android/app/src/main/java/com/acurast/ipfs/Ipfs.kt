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
 * A reusable IPFS client, safe to share between coroutines. The node is started
 * on the first download and reused by later ones.
 *
 * [idleTimeout] shuts the node down once it has been unused for that long,
 * restarting it on the next download; `null` keeps it up until [close].
 *
 * [delegatedRouting] queries a delegated routing v1 endpoint such as
 * `https://cid.contact` alongside the DHT. It finds content that is indexed but
 * never announced to the DHT; in exchange the endpoint learns which CIDs this
 * device fetches. Off by default.
 *
 * [gateways] are HTTP gateways fetched from alongside libp2p peers and verified
 * like any other source.
 *
 * [allowUnverifiedGatewayFallback] permits a whole-file fetch from those
 * gateways once every verified route has failed. Such a response cannot be
 * checked against its CID and is trusted because the gateway served it. Enable
 * it when unverified content still beats none.
 */
public class Ipfs(
    private val bootstrapNodes: List<String> = emptyList(),
    private val port: Int = PORT,
    private val idleTimeout: Duration? = IDLE_TIMEOUT,
    private val delegatedRouting: String? = null,
    private val gateways: List<String> = emptyList(),
    private val allowUnverifiedGatewayFallback: Boolean = false,
) : Closeable {

    // A monitor rather than a coroutine Mutex: close() overrides
    // Closeable.close() and so cannot suspend, yet it guards the same state
    // client() does. Mutex.withLock needs a suspend context, and runBlocking in
    // close() can deadlock on an exhausted dispatcher - which `use { }` supplies.
    //
    // Only correct while the guarded blocks never suspend, since a monitor
    // belongs to a thread and a coroutine may resume on another. Keep them free
    // of I/O; needing to await in here means dropping Closeable for a suspend
    // close() and switching to a Mutex.
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

        // Outside the lock: teardown waits on libp2p, bitswap and the DHT, and
        // holding the monitor across it would block every concurrent get().
        running?.close()
    }

    private fun client(): FfiClient = synchronized(lock) {
        if (closed) throw IOException("IPFS client is closed")

        // Only validates the configured lists; the node comes up on the first
        // download.
        client ?: Ffi.newClient(
            bootstrapNodes.joinToString(DELIMITER_LIST_STRING),
            port,
            idleTimeout?.inWholeMilliseconds ?: NO_TIMEOUT,
            delegatedRouting ?: NO_DELEGATED_ROUTING,
            gateways.joinToString(DELIMITER_LIST_STRING),
            allowUnverifiedGatewayFallback,
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
         * short enough that an idle app stops holding sockets open.
         */
        private val IDLE_TIMEOUT: Duration = 30.seconds

        /** The FFI encodes "no limit" and "no timeout" as negative, "no endpoint" as empty. */
        private const val NO_SIZE_LIMIT = -1L
        private const val NO_TIMEOUT = -1L
        private const val NO_DELEGATED_ROUTING = ""

        private const val DIR_IPFS = "ipfs"
        private const val DIR_DATA = "data"

        private const val DELIMITER_LIST_STRING = ";"
    }
}
