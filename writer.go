package bufiog

import "errors"

var errNegativeWrite = errors.New("bufiog: writer returned negative count from Write")

type WriteInterface[T any] interface {
	Write(p []T) (int, error)
}

type WriteToInterface[T any] interface {
	WriteTo(WriteInterface[T]) (int64, error)
}
