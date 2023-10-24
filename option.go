package redisc

type pipeOptions struct {
	transaction bool
	readOnly    bool
}

type PipeOption struct {
	f func(*pipeOptions)
}

func EnableTransaction() PipeOption {
	return PipeOption{func(po *pipeOptions) {
		po.transaction = true
	}}
}

func EnableReadOnly() PipeOption {
	return PipeOption{func(po *pipeOptions) {
		po.readOnly = true
	}}
}
