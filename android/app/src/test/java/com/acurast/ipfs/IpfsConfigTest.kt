package com.acurast.ipfs

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Test
import kotlin.time.Duration.Companion.seconds

/**
 * Construction only: the node, and so the native library, is not touched until
 * the first download.
 */
class IpfsConfigTest {

    @Test
    fun groupedConfiguration() {
        Ipfs(
            routing = Ipfs.Routing(
                bootstrapNodes = listOf("/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN"),
                delegated = "https://cid.contact",
            ),
            gateways = Ipfs.Gateways(
                urls = listOf("https://ipfs.io"),
                allowUnverifiedFallback = true,
            ),
            timeouts = Ipfs.Timeouts(
                idle = 30.seconds,
                primary = 10.seconds,
                fallbackStep = 5.seconds,
            ),
        )
    }

    /**
     * What the `Ipfs(Context, ...)` factory does with the resolvers it reads, minus
     * the reading, which needs a device.
     */
    @Test
    fun configuredDnsServersComeBeforeTheOnesAddedToThem() {
        val configured = Ipfs.Routing(dnsServers = listOf("1.1.1.1"))

        assertEquals(
            listOf("1.1.1.1", "192.168.1.1"),
            configured.withDnsServers(listOf("192.168.1.1", "1.1.1.1")).dnsServers,
        )
    }

    @Test
    fun addedDnsServersAreUsedWhenNoneWereConfigured() {
        assertEquals(
            listOf("192.168.1.1"),
            Ipfs.Routing().withDnsServers(listOf("192.168.1.1")).dnsServers,
        )
    }

    /** The plain constructor stays free of any device lookup. */
    @Test
    fun theConstructorReadsNoResolvers() {
        assertEquals(emptyList<String>(), Ipfs.Routing().dnsServers)
    }

    @Test
    fun defaultsAreConservative() {
        assertEquals(30.seconds, Ipfs.Timeouts().idle)

        // Both defer to the client's own bound rather than inventing one here.
        assertNull(Ipfs.Timeouts().primary)
        assertNull(Ipfs.Timeouts().fallbackStep)

        // Unverified content is opt in, and no endpoint is contacted unless named.
        assertFalse(Ipfs.Gateways().allowUnverifiedFallback)
        assertNull(Ipfs.Routing().delegated)
    }
}
