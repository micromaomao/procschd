export class SomethingNotFoundError extends Error {
  constructor (msg: string) {
    super(msg)
  }
}

export class ImageNotFoundError extends SomethingNotFoundError {
  imageId: string

  constructor(imageId: string) {
    super(`Image ${imageId} not found.`)
    this.imageId = imageId
  }
}

export class TaskNotFoundError extends SomethingNotFoundError {
  taskId: number

  constructor(taskId: number) {
    super(`Task ${taskId} not found.`)
    this.taskId = taskId
  }
}

export class TaskNotCompletedError extends Error {
  taskId: number

  constructor(taskId: number) {
    super(`Task ${taskId} not completed yet.`)
    this.taskId = taskId
  }
}

export class ArtifactNotFoundError extends Error {
  taskId: number
  artifactPath: string

  constructor(taskId: number, path: string) {
    super(`Artifact ${path} does not exist for task ${taskId}`)
    this.taskId = taskId
    this.artifactPath = path
  }
}
