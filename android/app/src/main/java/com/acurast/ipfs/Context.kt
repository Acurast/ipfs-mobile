package com.acurast.ipfs

import android.content.Context
import android.net.ConnectivityManager
import androidx.core.content.getSystemService
import java.io.File

private const val DIR_IPFS = "ipfs"
private const val DIR_DATA = "data"
private const val DIR_STATE = "state"
private const val FILE_PEER_SNAPSHOT = "peers"

private val Context.ipfsDir: File
    get() = File(dataDir, DIR_IPFS).apply {
        if (!exists()) mkdir()
    }

private fun Context.ipfsDir(child: String): File =
    File(ipfsDir, child).apply {
        if (!exists()) mkdirs()
    }

/** Where a download lands when the caller does not name a file itself. */
internal val Context.ipfsDataDir: File
    get() = ipfsDir(DIR_DATA)

/**
 * Where the client records the peers it met, so the next node starts by dialling
 * peers already known to hold up instead of walking the DHT to find them again.
 *
 * Under the app's own data directory: it holds no content, only where the network
 * was last seen, and is safe to lose.
 */
internal val Context.peerSnapshotPath: String
    get() = File(ipfsDir(DIR_STATE), FILE_PEER_SNAPSHOT).path

/**
 * The resolvers the active network is using, empty when there is no network or
 * they cannot be read.
 */
internal val Context.dnsServers: List<String>
    get() {
        val manager = getSystemService<ConnectivityManager>() ?: return emptyList()

        // The manifest asks for ACCESS_NETWORK_STATE, but an app is free to drop
        // what a library merged into it.
        return try {
            val active = manager.activeNetwork ?: return emptyList()
            val properties = manager.getLinkProperties(active) ?: return emptyList()

            properties.dnsServers.mapNotNull { it.hostAddress }
        } catch (e: SecurityException) {
            emptyList()
        }
    }
