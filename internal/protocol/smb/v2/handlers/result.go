package handlers

// HandlerResult contains the response data and status
type HandlerResult struct {
	Data   []byte // Response body (after header)
	Status uint32 // NT_STATUS code
}

// NewResult creates a new handler result
func NewResult(status uint32, data []byte) *HandlerResult {
	return &HandlerResult{
		Status: status,
		Data:   data,
	}
}

// NewErrorResult creates an error result with the given status and no data
func NewErrorResult(status uint32) *HandlerResult {
	return &HandlerResult{
		Status: status,
		Data:   nil,
	}
}
