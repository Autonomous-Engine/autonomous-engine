export function createApp(config: AppConfig): App {
  return new App(config);
}

export class UserService {
  getUser(id: string) { return null; }
}

export const DEFAULT_CONFIG = { port: 3000 };

export type AppConfig = {
  port: number;
  host?: string;
};

export interface Logger {
  info(msg: string): void;
  error(msg: string): void;
}
