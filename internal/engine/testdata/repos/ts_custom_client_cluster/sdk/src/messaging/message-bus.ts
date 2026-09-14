import { Injectable } from "@nestjs/common";

export interface QueueTransport {
  sendRequest(queue: string, path: string, options: { method: string }): Promise<void>;
}

// The precision control. Same method name, same argument shape, a path a server
// really serves, but the receiver is not a configured client type. It must never
// become a client route: the receiver's declared type decides, not the method name.
@Injectable()
export class MessageBus {
  constructor(private readonly transport: QueueTransport) {}

  async publishImport(): Promise<void> {
    await this.transport.sendRequest("imports", "/v1/catalog/imports", { method: "POST" });
  }
}
