package com.acurast.ipfs

import android.content.Context
import android.net.ConnectivityManager

/**
 * The resolvers the active network is using, empty when there is no network or
 * they cannot be read.
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
