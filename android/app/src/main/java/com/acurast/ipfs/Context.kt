package com.acurast.ipfs

import android.content.Context
import android.net.ConnectivityManager

/**
 * The resolvers the active network is using.
 *
 * Empty when there is no network, when they cannot be read, or on a device that
 * does not report them, which leaves `/dnsaddr/` bootstrap addresses unresolvable
 * unless [Ipfs.Routing.dnsServers] names something.
 */
internal val Context.dnsServers: List<String>
    get() {
        val manager = getSystemService(ConnectivityManager::class.java) ?: return emptyList()
        val active = manager.activeNetwork ?: return emptyList()

        // The manifest asks for ACCESS_NETWORK_STATE, but an app is free to drop
        // what a library merged into it.
        val properties = try {
            manager.getLinkProperties(active)
        } catch (e: SecurityException) {
            null
        } ?: return emptyList()

        return properties.dnsServers.mapNotNull { it.hostAddress }
    }
