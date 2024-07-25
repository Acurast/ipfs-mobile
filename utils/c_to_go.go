package utils

import (
	"C"
	"unsafe"
)

func GoStringSlice(array unsafe.Pointer, length int32) []string {
	slice := make([]string, length)
	for i := 0; i < int(length); i++ {
		slice[i] = C.GoString((*C.char)(unsafe.Pointer(uintptr(array) + uintptr(i) * unsafe.Sizeof(*(**C.char)(array)))))
	}

	return slice
}