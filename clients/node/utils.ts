import * as http from 'http'
import { Readable } from 'stream';

export function readAll(s: http.IncomingMessage | Readable): Promise<Buffer> {
  let done: boolean = false
  let chunks: Array<Buffer> = []
  return new Promise((resolve, reject) => {
    s.on('data', data => {
      if (done) return
      chunks.push(data)
    })
    s.on('end', () => {
      if (done) return
      done = true
      if (s instanceof http.IncomingMessage) {
        if (!s.complete) {
          reject(new Error("Connection terminated permaturally."))
          return
        }
      }
      resolve(Buffer.concat(chunks))
    })
    s.on('error', err => {
      if (done) return
      done = true
      reject(err)
    })
  })
}
