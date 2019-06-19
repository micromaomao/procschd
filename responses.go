package main

type StatResponse struct {
	NumThreads        uint32 `json:"numThreads"`
	NumThreadsDesired uint32 `json:"numThreadsDesired"`
	NumTasksPending   int64  `json:"numTasksPending"`
}

type TaskResponse struct {
	Id        uint64   `json:"id"`
	Completed bool     `json:"completed"`
	Error     string   `json:"error,omitempty"`
	Stdout    string   `json:"stdout"`
	Stderr    string   `json:"stderr"`
	Artifacts []string `json:"artifacts"`
}
