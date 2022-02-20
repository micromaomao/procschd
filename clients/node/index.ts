import * as http from 'http';
import * as https from 'https';
import { Readable, Writable } from 'stream';
import { URL } from 'url';
import { ArtifactNotFoundError, ImageNotFoundError, SomethingNotFoundError, TaskNotCompletedError, TaskNotFoundError } from './errors';
import { StatResponse, TaskInfo } from './responses';
import { readAll } from './utils';
import crypto = require('crypto')

export * from './errors';
export * from './responses';

interface RunTaskOptions {
  imageId: string
  stdin?: string | Buffer | Readable | undefined
  fetchArtifacts?: Array<string> | undefined
  uploadAssets?: Array<Upload> | undefined
}

interface Upload {
  /**
   * An absolute path to place the file into.
   */
  targetPath: string,
  data: string | Buffer | Readable
}

export class ProcschdServer {
  protected httpAgent: http.Agent | https.Agent
  protected mode: "tcp" | "unix"
  protected protocol: "http" | "https"
  protected baseUrl: URL | null = null
  protected socketPath: string | null = null
  protected authToken: string = ""

  protected constructor(mode: "tcp" | "unix", proto: "http" | "https") {
    this.protocol = proto
    if (proto == "https") {
      this.httpAgent = new https.Agent({
        keepAlive: true,
        maxSockets: Infinity,
        timeout: 1000
      })
    } else {
      this.httpAgent = new http.Agent({
        keepAlive: true,
        maxSockets: Infinity,
        timeout: 1000
      })
    }
    this.mode = mode
  }

  /**
   * Connect to a running procschd instance via TCP
   * @param baseUrl e.g. "http://127.0.0.1:8888"
   * @param authToken the string passed as --auth-token to the server. Default: ""
   */
  static async connect(baseUrl: URL | string, authToken: string = ""): Promise<ProcschdServer> {
    if (typeof baseUrl === "string") {
      baseUrl = new URL(baseUrl)
    }
    let protocol = baseUrl.protocol.replace(/:$/, "");
    if (protocol != "http" && protocol != "https") {
      throw new Error("Invalid protocol.");
    }
    let schd = new ProcschdServer("tcp", protocol);
    schd.baseUrl = baseUrl
    schd.authToken = authToken
    await schd.stat()
    return schd
  }

  /**
   * Simillar to connect, but via unix socket.
   * @param socketPath path of socket file, by default it is "/run/procschd.sock"
   * @param authToken the string passed as --auth-token to the server. Default: ""
   */
  static async unixConnect(socketPath: string = "/run/procschd.sock", authToken: string = ""): Promise<ProcschdServer> {
    let schd = new ProcschdServer("unix", "http")
    schd.socketPath = socketPath
    schd.authToken = authToken
    await schd.stat()
    return schd
  }

  /**
   * Send a raw request to the server. Application code usually don't need to call this. For more information, see node http.request
   */
  request(path: string, options: http.RequestOptions, callback?: (res: http.IncomingMessage) => void): http.ClientRequest {
    options = Object.assign({}, options)
    options.agent = this.httpAgent;
    options.headers = options.headers || {}
    if (this.authToken.length > 0) {
      options.headers["authorization"] = "Bearer " + this.authToken
    }
    if (!options.headers["user-agent"]) {
      options.headers["user-agent"] = "nodejs"
    }
    switch (this.mode) {
      case "tcp":
        if (this.baseUrl === null) throw "!"
        let realUrl = new URL(path, this.baseUrl)
        if (this.protocol == "http") {
          return http.request(realUrl, options, callback)
        } else {
          return https.request(realUrl, options, callback)
        }
        break
      case "unix":
        if (this.socketPath === null) throw "!"
        options.socketPath = this.socketPath
        options.path = options.path || path
        return http.request(options, callback)
        break
    }
  }

  /**
   * Send a GET request to the server and parse the response as a JSON object. Application code usually don't need to call this.
   */
  getJson(path: string): Promise<any> {
    return new Promise((resolve, reject) => {
      this.request(path, { method: "GET" }, async icm => {
        try {
          let bodyStr = (await readAll(icm)).toString('utf-8')
          if (icm.statusCode !== 200) {
            if (icm.statusCode === 404) {
              throw new SomethingNotFoundError(`${icm.statusMessage}: ${bodyStr}`)
            }
            throw new Error(`${icm.statusCode} ${icm.statusMessage}: ${bodyStr}`)
          }
          if (!(icm.headers["content-type"] || "").endsWith("/json")) throw new Error(`Invalid Content-Type ${icm.headers["content-type"]}, expected some json.`)
          let body = JSON.parse(bodyStr)
          resolve(body)
        } catch (e) {
          reject(e)
        }
      }).end()
    })
  }

  /**
   * Get some basic info from the server.
   */
  async stat(): Promise<StatResponse> {
    let res = await this.getJson("/stat")
    return res
  }

  /**
   * Determine the date of a Docker image on the server.
   * @param imageId e.g. runner/demo
   */
  async imageDate(imageId: string): Promise<Date> {
    return new Promise((resolve, reject) => {
      this.request("/image?id=" + encodeURIComponent(imageId), { method: 'GET' }, async icm => {
        try {
          let body = (await readAll(icm)).toString('utf8')
          if (icm.statusCode !== 200) {
            if (icm.statusCode === 404) {
              throw new ImageNotFoundError(imageId)
            }
            throw new Error(`${icm.statusCode} ${icm.statusMessage}: ${body}`)
          }
          let date = new Date(body.trim())
          if (Number.isNaN(date.getTime())) {
            throw new Error("Server returned " + body + ", which is an invalid date.")
          }
          resolve(date)
        } catch (e) {
          reject(e)
        }
      }).end()
    })
  }

  /**
   * Get information about a task.
   * @param id task id as returned by .runTask (or other helper functions)
   * @param waitForCompletion whether or not to wait for the task to complete before getting the info.
   *
   * It is advised that even with waitForCompletion set to true, the caller still checks TaskInfo::complete and retry if not.
   */
  async taskInfo(id: number, waitForCompletion: boolean): Promise<TaskInfo> {
    // TODO: Maybe re-connect every 30s to prevent connection staling?
    try {
      let body = await this.getJson('/task?id=' + id.toString() + (waitForCompletion ? '&wait=1' : ''))
      return body
    } catch (e) {
      if (e instanceof SomethingNotFoundError) {
        throw new TaskNotFoundError(id)
      } else {
        throw e
      }
    }
  }

  /**
   * Download an artifact, after a task successfully completes.
   *
   * Caller is responsible for consuming the returned Readable. Call .resume() if data is not needed.
   *
   * Read IncomingMessage::headers["content-length"] for length info.
   */
  async downloadArtifact(taskId: number, artifactPath: string): Promise<http.IncomingMessage> {
    return new Promise((resolve, reject) => {
      this.request(`/task/artifact?id=${taskId}&path=${artifactPath}`, { method: 'GET' }, async icm => {
        if (icm.statusCode !== 200) {
          let response = (await readAll(icm)).toString('utf8').trim()
          if (icm.statusCode === 404) {
            if (response === "No such task.")
              return void reject(new TaskNotFoundError(taskId))
            if (response === "Task not completed.")
              return void reject(new TaskNotCompletedError(taskId))
            if (response === "No such artifact.")
              return void reject(new ArtifactNotFoundError(taskId, artifactPath))
          }
          reject(new Error(`${icm.statusCode} ${icm.statusMessage}: ${response}.`))
          return
        }
        resolve(icm)
      }).end()
    })
  }

  /**
   * Schedule a task to run.
   *
   * This method returns immediately after server acknowledges the task. To wait for task to complete, call taskInfo with waitForCompletion = true,
   * or use other helper functions.
   *
   * To be able to download artifacts later, set options.fetchArtifacts to be an array of paths. If the task succeed, artifacts can be
   * downloaded with downloadArtifact. Path must be absolute. e.g. fetchArtifacts: ["/tmp/out.pdf"]
   *
   * To upload files to the executation environment, set options.uploadAssets
   *
   * @param options
   */
  async runTask(options: RunTaskOptions): Promise<number> {
    if (typeof options.fetchArtifacts === "undefined") {
      options.fetchArtifacts = []
    }
    if (typeof options.uploadAssets === "undefined") {
      options.uploadAssets = []
    }
    async function generateBoundary(): Promise<string> {
      return new Promise((resolve, reject) => {
        crypto.randomBytes(16, (err, buf) => {
          if (err) {
            reject(err)
          } else {
            resolve('-------------' + buf.toString('hex'))
          }
        })
      })
    }
    let boundary = await generateBoundary()
    let req = this.request('/task', {
      method: 'POST',
      headers: {
        'content-type': 'multipart/form-data; boundary=' + boundary,
        'transfer-encoding': 'chunked'
      }
    })
    function reqWrite(chunk: string | Buffer): Promise<void> {
      return new Promise((resolve, reject) => {
        if (typeof chunk === 'string') chunk = Buffer.from(chunk, 'utf8')
        if (chunk.byteLength === 0) return void resolve()
        // let chunked_chunk = Buffer.concat([Buffer.from(`${chunk.byteLength.toString(16)}\r\n`), chunk, Buffer.from('\r\n', 'utf8')])
        // Turns out nodejs does chunk encoding for us!
        if (req.write(chunk)) {
          resolve()
        } else {
          req.once('drain', () => {
            resolve()
            req.off('error', reject)
          })
          req.once('error', reject)
        }
      })
    }
    function reqPipe(readable: Readable): Promise<void> {
      return new Promise((resolve, reject) => {
        let ended = false
        readable.once('end', () => {
          if (ended) return
          ended = true
          resolve()
        })
        readable.once('error', err => {
          if (ended) return
          ended = true
          reject(err)
        })
        readable.pipe(req, { end: false })
      })
    }
    async function writeFormValue(name: string, value: string | Buffer) {
      if (typeof value === "string") {
        await reqWrite(`--${boundary}\r\nContent-Disposition: form-data; name=${JSON.stringify(name)}\r\n\r\n${value}\r\n`)
      } else {
        await reqWrite(Buffer.concat([Buffer.from(`--${boundary}\r\nContent-Disposition: form-data; name=${JSON.stringify(name)}; filename=${JSON.stringify(name)}\r\n\r\n`, 'utf8'), value, Buffer.from('\r\n', 'utf8')]))
      }
    }
    await writeFormValue("imageId", options.imageId)
    for (let a of options.fetchArtifacts) {
      await writeFormValue("artifacts", a)
    }
    if (typeof options.stdin === "string" || options.stdin instanceof Buffer) {
      await writeFormValue("stdin", options.stdin)
    } else if (options.stdin instanceof Readable) {
      await reqWrite(`--${boundary}\r\nContent-Disposition: form-data; name="stdin"; filename="stdin"\r\n\r\n`)
      await reqPipe(options.stdin)
      await reqWrite('\r\n')
    } else {
      // no stdin provided.
    }
    for (let upload of options.uploadAssets) {
      await reqWrite(`--${boundary}\r\nContent-Disposition: form-data; name="uploads"; filename=${JSON.stringify(upload.targetPath)}\r\n\r\n`)
      if (typeof upload.data === "string" || upload.data instanceof Buffer) {
        await reqWrite(upload.data)
      } else {
        await reqPipe(upload.data)
      }
      await reqWrite('\r\n')
    }
    await reqWrite(`--${boundary}--\r\n`)
    let icm = await (function (): Promise<http.IncomingMessage> {
      return new Promise<http.IncomingMessage>((resolve, reject) => {
        req.end()
        req.on('response', icm => {
          resolve(icm)
        })
      })
    })()
    let body = (await readAll(icm)).toString('utf8')
    if (icm.statusCode !== 200) {
      throw new Error(`${icm.statusCode} ${icm.statusMessage}: ${body}`)
    } else {
      return parseInt(body)
    }
  }
}
