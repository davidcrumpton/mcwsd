/**
 * mock-host.ts — test double for the mcwsd runtime plugin host global.
 * Injected as globalThis.host before each test via installMockHost.
 */

import { vi } from "vitest";
import type { CommandResult, Host } from "../types/mcwsd-runtime";

export function buildMockHost(options: {
  options?: Record<string, unknown>;
  run?: (argv: string[]) => CommandResult;
} = {}): Host {
  const defaultRun = () => ({ stdout: "", stderr: "", exit_code: 0 });

  return {
    run: options.run ? (argv: string[]) => options.run!(argv) : vi.fn(defaultRun),
    sleep: vi.fn(() => {}),
    log: vi.fn(() => {}),
    env: vi.fn((_key: string) => ""),
    onFail: vi.fn(() => {}),
    options: options.options ?? {},
    ipam: {
      allocate: vi.fn(() => "192.168.1.10/24"),
      release: vi.fn(() => {}),
      reserve: vi.fn(() => {}),
      gateway: vi.fn(() => "192.168.1.1"),
      network: vi.fn(() => "192.168.1.0/24"),
    },
    fetch: vi.fn(() => ""),
    fs: {
      write: vi.fn(() => {}),
      read: vi.fn(() => ""),
    },
  };
}

export function installMockHost(options?: Parameters<typeof buildMockHost>[0]): Host {
  const mockHost = buildMockHost(options);
  // @ts-expect-error — injecting the goja host global for plugin tests
  globalThis.host = mockHost;
  return mockHost;
}
