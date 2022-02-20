import * as mocha from 'mocha';
import { ArtifactNotFoundError, ImageNotFoundError, ProcschdServer, TaskNotFoundError } from "../index";
import { readAll } from '../utils';
import assert = require('assert')
import stream = require('stream')

function annotate(text: string) {
  console.log(`    \x1b[90m(${text})\x1b[0m`)
}

let unixSocketPath = process.env.UNIX_SOCKET_PATH
let httpUrl = process.env.HTTP_URL
let authToken = process.env.AUTH_TOKEN || ""

let pHttp: ProcschdServer | null = null
let pSocket: ProcschdServer | null = null

  ; (httpUrl ? it : it.skip)('connects via TCP', async function () {
    if (typeof httpUrl !== "string") throw new Error("!")
    let server = await ProcschdServer.connect(httpUrl, authToken)
    pHttp = server
  })

  ; (unixSocketPath ? it : it.skip)('connects via unix socket', async function () {
    if (typeof unixSocketPath !== "string") throw new Error("!")
    let server = await ProcschdServer.unixConnect(unixSocketPath, authToken)
    pSocket = server
  })

before(async () => {
  if (!unixSocketPath && !httpUrl) {
    console.error("No connection is set up!")
    console.error("  Set at least one of UNIX_SOCKET_PATH or HTTP_URL")
    return Promise.reject(new Error("No server to connect to."))
  }
  return Promise.resolve({})
})

function itOverServers(desc: string, testFn: (this: mocha.Context, s: ProcschdServer) => Promise<void>): void {
  if (httpUrl) {
    it(desc + " (over tcp)", async function () {
      this.timeout(3000)
      if (!pHttp) throw new Error("Connection not available.")
      await testFn.call(this, pHttp)
    })
  }
  if (unixSocketPath) {
    it(desc + " (over unix socket)", async function () {
      if (!pSocket) throw new Error("Connection not available.")
      await testFn.call(this, pSocket)
    })
  }
}

function itOverOneServer(desc: string, testFn: (this: mocha.Context, s: ProcschdServer) => Promise<void>): void {
  if (unixSocketPath) {
    it(desc, async function () {
      if (!pSocket) throw new Error("Connection not available")
      await testFn.call(this, pSocket)
    })
  } else if (httpUrl) {
    it(desc, async function () {
      this.timeout(3000)
      if (!pHttp) throw new Error("Connection not available")
      await testFn.call(this, pHttp)
    })
  }
}


itOverServers('/stat responses sanely', async s => {
  let res = await s.stat()
  if (!res || !Number.isSafeInteger(res.numThreads) || !Number.isSafeInteger(res.numThreadsDesired) || !Number.isSafeInteger(res.numTasksPending)
    || res.numThreads <= 0 || res.numThreadsDesired <= 0 || res.numTasksPending < 0) {
    throw new Error("Invalid response.")
  }
  annotate("Response was " + JSON.stringify(res))
})

itOverServers('/images responses sanely', async s => {
  let d = await s.imageDate("runner/demo")
  if (d < new Date('2019-01-01')) {
    throw new Error(`Date returned was ${d}, which is obviously wrong.`)
  }
})

itOverServers('Check existence of needed images', async s => {
  for (let i of ["runner/demo", "runner/bash"]) {
    await s.imageDate(i)
  }
})

itOverServers('Should error for non-existing images', async s => {
  try {
    await s.imageDate("404/not-found")
  } catch (e) {
    if (e instanceof ImageNotFoundError) {
      return // good
    } else {
      throw new Error("Image lookup failed with error " + e)
    }
  }
  throw new Error("No error thrown")
})

itOverServers('/task should 404 for non-existing tasks', async s => {
  try {
    await s.taskInfo(0, false)
  } catch (e) {
    if (e instanceof TaskNotFoundError) {
      return
    } else {
      throw new Error("Task lookup failed with error " + e)
    }
  }
  throw new Error("No error thrown")
})

itOverServers('/task/artifact should 404 for non-existing tasks with response = "No such task."', async s => {
  try {
    await s.downloadArtifact(0, '/tmp/')
  } catch (e) {
    if (e instanceof TaskNotFoundError) {
      return
    } else {
      throw new Error("Artifact download failed with error " + e)
    }
  }
  throw new Error("No error thrown")
})

let basicTaskId: number = 0

itOverOneServer('runTask basic', async function (s) {
  this.timeout(5000)
  let taskId = await s.runTask({
    "imageId": "runner/demo",
    "stdin": "hello"
  })
  basicTaskId = taskId
  annotate(`taskId = ${taskId}`)
  let taskInfo = await s.taskInfo(taskId, true)
  if (taskInfo.error) {
    throw new Error("Task failed to run: " + taskInfo.error)
  }
  if (!taskInfo.stdout.match(/00000000: 6865 6c6c 6f +hello/)) {
    throw new Error(`stdout = ${JSON.stringify(taskInfo.stdout)}, which is not expected.`)
  }
  assert.strictEqual(taskInfo.id, taskId)
  assert.strictEqual(taskInfo.completed, true)
  assert.strictEqual(taskInfo.stderr, "")
  assert.strictEqual(taskInfo.artifacts.length, 0)
})

itOverServers('/task/artifact should 404 for non-existing artifacts on existing task with response = "No such artifact."', async s => {
  try {
    await s.downloadArtifact(basicTaskId, '/tmp/')
  } catch (e) {
    if (e instanceof ArtifactNotFoundError) {
      return
    } else {
      throw new Error("Artifact download failed with error " + e)
    }
  }
  throw new Error("No error thrown")
})

async function assertTaskStdoutAndNoStderr(s: ProcschdServer, taskId: number, expectedStdout: string): Promise<void> {
  let info = await s.taskInfo(taskId, true)
  if (info.error) {
    throw new Error(`Task failed: ${info.error}`)
  }
  assert.strictEqual(info.completed, true)
  assert.strictEqual(info.stdout, expectedStdout)
  assert.strictEqual(info.stderr, "")
}

itOverServers('runTask works for Buffer stdin', async function (s) {
  this.timeout(5000)
  let taskId = await s.runTask({
    imageId: "runner/bash",
    stdin: Buffer.from("echo hello!", 'utf8')
  })
  await assertTaskStdoutAndNoStderr(s, taskId, "hello!\n")
})

itOverServers('runTask works for Stream stdin', async function (s) {
  this.timeout(5000)
  let i: number = 0
  let st = new stream.Readable({
    autoDestroy: true,
    read: function (size: number): void {
      if (i === 0) {
        this.push("# ", 'utf8')
        i++
      } else if (i < 100) {
        this.push("this is a long comment ", 'utf8')
        i++
      } else if (i === 100) {
        this.push("\necho Hello world!\n")
        i++
      } else {
        this.push(null)
      }
    }
  })
  // To check the validity of the above stream implementation:
  // st.pipe(process.stdout)
  let taskId = await s.runTask({
    imageId: 'runner/bash',
    stdin: st
  })
  await assertTaskStdoutAndNoStderr(s, taskId, "Hello world!\n")
})


itOverServers('Artifact download', async s => {
  let taskId = await s.runTask({
    imageId: 'runner/bash',
    stdin: 'echo Artifact! > /tmp/artifact',
    fetchArtifacts: ['/tmp/artifact']
  })
  await assertTaskStdoutAndNoStderr(s, taskId, "")
  let artifactStream = await s.downloadArtifact(taskId, '/tmp/artifact')
  let read = (await readAll(artifactStream)).toString('utf8')
  assert.strictEqual(read, "Artifact!\n")
})

itOverServers('Test sha256sum with asset upload via string', async s => {
  let taskId = await s.runTask({
    imageId: 'runner/bash',
    stdin: "sha256sum /tmp/upload | cut -f1 -d' '",
    uploadAssets: [
      {
        targetPath: '/tmp/upload',
        data: 'I like ASCII'
      }
    ]
  })
  await assertTaskStdoutAndNoStderr(s, taskId, 'c2eb9065fa04af99d13973087e9b36f50225e2b101c238f82e0c05872eb3524f\n')
})

itOverServers('Test sha256sum with asset upload via Buffer', async s => {
  let taskId = await s.runTask({
    imageId: 'runner/bash',
    stdin: "sha256sum /tmp/upload | cut -f1 -d' '",
    uploadAssets: [
      {
        targetPath: '/tmp/upload',
        data: Buffer.from(new Uint8Array([0, 1, 2, 3, 4, 5, 6]))
      }
    ]
  })
  await assertTaskStdoutAndNoStderr(s, taskId, '57355ac3303c148f11aef7cb179456b9232cde33a818dfda2c2fcb9325749a6b\n')
})

import crypto = require('crypto')

itOverServers('Test large asset upload via Stream', async function (s) {
  this.timeout(40000)
  const sha256 = crypto.createHash('sha256')
  let bytesLeft = 100 * (1<<20)
  let expectHash: string | null = null
  let randomStream = new stream.Readable({
    autoDestroy: true,
    read: function (size: number): void {
      if (bytesLeft > 0) {
        process.stderr.write(`\x1b[90m\x1b[2K\r    ${bytesLeft >> 20} MB left to send...\x1b[0m`)
        let numBytesToGenerate = Math.min(size, bytesLeft)
        bytesLeft -= numBytesToGenerate
        crypto.randomBytes(numBytesToGenerate, (err, buf) => {
          if (err) {
            this.emit('error', err)
            return
          }
          sha256.update(buf)
          this.push(buf)
        })
      } else {
        process.stderr.write(`\x1b[90m\x1b[2K\r    Upload done.\r\x1b[0m`)
        expectHash = sha256.digest('hex')
        this.push(null)
      }
    }
  })
  let taskId = await s.runTask({
    imageId: 'runner/bash',
    stdin: "sha256sum /tmp/upload | cut -f1 -d' '",
    uploadAssets: [
      {
        targetPath: '/tmp/upload',
        data: randomStream
      }
    ]
  })
  if (expectHash === null) {
    throw new Error("expectHash not calculated?")
  }
  await assertTaskStdoutAndNoStderr(s, taskId, expectHash + '\n')
})

itOverServers('non-zero exit code should cause error', async s => {
  let info = await s.taskInfo(await s.runTask({
    imageId: "runner/bash",
    stdin: "false"
  }), true)
  if (!info.error) {
    throw new Error("Expected error to present.")
  }
})

itOverServers('timeout should cause error', async function (s) {
  this.timeout(30000)
  let info = await s.taskInfo(await s.runTask({
    imageId: "runner/bash",
    stdin: "sleep 100"
  }), true)
  if (!info.error) {
    throw new Error("Expected error to present.")
  }
  if (!info.completed) {
    throw new Error("Expected task.completed = true")
  }
})

itOverServers('if waitForCompletion is false, should not wait.', async s => {
  let info = await s.taskInfo(await s.runTask({
    imageId: "runner/bash",
    stdin: "sleep 100"
  }), false)
  if (info.completed) {
    throw new Error("Expected task.completed = false")
  }
})
