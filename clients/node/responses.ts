export interface StatResponse {
  numThreads: number
  numThreadsDesired: number
  numTasksPending: number
}

export interface TaskInfo {
  id: number
  completed: boolean
  error: string
  stdout: string
  stderr: string
  artifacts: Array<string>
}
