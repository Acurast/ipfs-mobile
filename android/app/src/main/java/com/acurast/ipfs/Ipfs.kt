package com.acurast.ipfs

import android.content.Context
import ffi.Config
import ffi.Ffi
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File
import java.io.IOException
import kotlin.time.Duration

public class Ipfs(private val bootstrapNodes: List<String> = emptyList(), private val port: Int = PORT) {

    public suspend fun get(
        context: Context,
        cid: String,
        output: File? = null,
        timeout: Duration? = null,
    ): File = withContext(Dispatchers.IO) {
        try {
            val output = output ?: File(context.ipfsDataDir, cid)

            Ffi.get(cid, output.absolutePath, Config().also {
                it.bootstrapPeers = bootstrapNodes.joinToString(DELIMITER_LIST_STRING)
                it.port = port
                it.timeout = timeout?.inWholeMilliseconds ?: -1L
            })

            output
        } catch (e: Throwable) {
            throw IOException(e.message, e.cause)
        }
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

        private const val DIR_IPFS = "ipfs"
        private const val DIR_DATA = "data"

        private const val DELIMITER_LIST_STRING = ";"
    }
}