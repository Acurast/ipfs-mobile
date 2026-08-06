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
import ffi.ClientConfig

/**
 * An [Ipfs] that can resolve the `/dnsaddr/` addresses among its bootstrap peers,
 * by naming the resolvers the active network is using.
 *
 * They are read once, here, so a client outliving the network it was built on
 * keeps that network's resolvers.
 */
public fun Ipfs(
    context: Context,
    routing: Ipfs.Routing = Ipfs.Routing(),
    gateways: Ipfs.Gateways = Ipfs.Gateways(),
    timeouts: Ipfs.Timeouts = Ipfs.Timeouts(),
    port: Int = Ipfs.PORT,
): Ipfs = Ipfs(routing.withDnsServers(context.dnsServers), gateways, timeouts, port)

/** Bootstrap peers alone. */
public fun Ipfs(context: Context, bootstrapNodes: List<String>): Ipfs =
    Ipfs(context, routing = Ipfs.Routing(bootstrapNodes))

/**
 * A reusable IPFS client, safe to share between coroutines. The node is started
 * on the first download and reused by later ones, and [close] releases it.
 */
public class Ipfs internal constructor(
    private val routing: Routing = Routing(),
    private val gateways: Gateways = Gateways(),
    private val timeouts: Timeouts = Timeouts(),
    private val port: Int = PORT,
) : Closeable {

    /** How content is located. */
    public data class Routing(
        /** Peers dialled to join the network. */
        val bootstrapNodes: List<String> = emptyList(),

        /**
         * Whether to look providers up in the DHT. Off leaves only the peers named
         * here and whatever [delegated] finds.
         */
        val dht: Boolean = true,

        /**
         * A delegated routing v1 endpoint such as `https://cid.contact`, queried
         * alongside the DHT. It finds content that is indexed but never announced
         * to the DHT; in exchange the endpoint learns which CIDs this device
         * fetches. `null` disables it.
         */
        val delegated: String? = null,

        /**
         * Resolvers used to look up addresses, for example `1.1.1.1`. Port 53
         * unless one is given.
         *
         * Only `/dnsaddr/` entries in [bootstrapNodes] need these, and Android
         * publishes none of its own, so without them such an entry resolves to
         * nothing. The `Ipfs(Context, ...)` factory supplies the active network's;
         * name others here to stay reachable where those are not.
         */
        val dnsServers: List<String> = emptyList(),
    )

    /** HTTP gateways, and how far to trust them. */
    public data class Gateways(
        /**
         * Gateway URLs, for example `https://ipfs.io`, fetched from alongside
         * libp2p peers and verified like any other source.
         */
        val urls: List<String> = emptyList(),

        /**
         * Permits a whole-file fetch from [urls] once every verified route has
         * failed. Such a response cannot be checked against its CID and is trusted
         * because the gateway served it. Enable it when unverified content still
         * beats none.
         */
        val allowUnverifiedFallback: Boolean = false,
    )

    /** Bounds on how long the client spends before giving up. */
    public data class Timeouts(
        /**
         * Shuts the node down once it has been unused for this long, restarting it
         * on the next download. `null` keeps it up until [close].
         */
        val idle: Duration? = DEFAULT_IDLE,

        /**
         * The most the primary phase - peers and gateways, every block verified -
         * may take before [Gateways.allowUnverifiedFallback] is allowed to start.
         * A download's own timeout shortens this but cannot extend it, so some of
         * that timeout always survives for the fallback. `null` uses 15s.
         */
        val primary: Duration? = null,

        /**
         * The most any single gateway attempt in the fallback may take. Per
         * attempt rather than per phase, so dead gateways cannot starve the one
         * that would have answered. `null` uses 15s.
         */
        val fallbackStep: Duration? = null,
    ) {
        public companion object {
            /**
             * Long enough that a burst of downloads reuses one set of connections,
             * short enough that an idle app stops holding sockets open.
             */
            private val DEFAULT_IDLE: Duration = 30.seconds
        }
    }

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

    public suspend fun get(cid: String, output: File, sizeLimit: Long? = null, timeout: Duration? = null): File =
        withContext(Dispatchers.IO) {
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
                    e.message?.startsWith("size limit exceeded") == true -> throw SizeLimitExceededException(
                        e.message,
                        e.cause
                    )

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
            ClientConfig().also {
                it.bootstrapPeers = routing.bootstrapNodes.joinToString(DELIMITER_LIST_STRING)
                it.dnsServers = routing.dnsServers.joinToString(DELIMITER_LIST_STRING)
                it.disableDHT = !routing.dht
                it.delegatedRouting = routing.delegated ?: NO_DELEGATED_ROUTING
                it.gateways = gateways.urls.joinToString(DELIMITER_LIST_STRING)
                it.allowUnverifiedGatewayFallback = gateways.allowUnverifiedFallback
                it.idleTimeout = timeouts.idle?.inWholeMilliseconds ?: NO_TIMEOUT
                it.primaryTimeout = timeouts.primary?.inWholeMilliseconds ?: USE_DEFAULT
                it.fallbackStepTimeout = timeouts.fallbackStep?.inWholeMilliseconds ?: USE_DEFAULT
                it.port = port
            },
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
        internal const val PORT = -1

        /** The FFI encodes "no limit" and "no timeout" as negative, "no endpoint" as empty. */
        private const val NO_SIZE_LIMIT = -1L
        private const val NO_TIMEOUT = -1L
        private const val NO_DELEGATED_ROUTING = ""

        /** ...and defers to the client's own default on a non-positive duration. */
        private const val USE_DEFAULT = 0L

        private const val DIR_IPFS = "ipfs"
        private const val DIR_DATA = "data"

        private const val DELIMITER_LIST_STRING = ";"
    }
}

// Configured resolvers first, so a caller that named one is not left behind
// whatever the network reports.
internal fun Ipfs.Routing.withDnsServers(servers: List<String>): Ipfs.Routing =
    copy(dnsServers = (dnsServers + servers).distinct())
