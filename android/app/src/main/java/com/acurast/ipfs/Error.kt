package com.acurast.ipfs

import java.io.IOException

public class SizeLimitExceededException(message: String? = null, cause: Throwable? = null) : IOException(message, cause)