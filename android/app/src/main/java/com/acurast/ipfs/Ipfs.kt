package com.acurast.ipfs

import android.content.Context
import ffi.Config
import ffi.Ffi
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.io.File
import java.io.IOException
import kotlin.time.Duration

public class Ipfs(private val bootstrapNodes: List<String> = emptyList()) {

    public suspend fun get(
        context: Context,
        cid: String,
        output: File? = null,
        timeout: Duration? = null,
    ): File = withContext(Dispatchers.IO) {
        try {
            val output = output ?: File(context.ipfsDataDir, cid)

            Ffi.get(cid, output.absolutePath, Config().apply {
                bootstrapPeers = bootstrapNodes.joinToString(DELIMITER_LIST_STRING)
                plugins = context.ipfsPluginsDir.absolutePath
                repo = context.ipfsRepoDir.absolutePath
                timeoutMs = timeout?.inWholeMilliseconds ?: -1L
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

    private val Context.ipfsPluginsDir: File
        get() = ipfsDir(DIR_PLUGINS)

    private val Context.ipfsRepoDir: File
        get() = ipfsDir(DIR_REPO)

    private val Context.ipfsDataDir: File
        get() = ipfsDir(DIR_DATA)

    public companion object {
        private const val DIR_IPFS: String = "ipfs"
        private const val DIR_PLUGINS: String = "plugins"
        private const val DIR_REPO: String = "repo"
        private const val DIR_DATA: String = "data"

        private const val DELIMITER_LIST_STRING = ";"
    }
}