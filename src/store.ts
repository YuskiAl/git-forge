// The forge's whole store: repositories, git objects, issues, pull requests,
// labels and comments, all in memory. Nothing here reads a wall clock or
// Math.random: every timestamp comes from a deterministic sequence counter
// (seq), so two runs fed the same requests in the same order produce the
// same state, byte for byte.

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: GitObjectType;
  data: Buffer;
}

export interface Label {
  name: string;
  color: string;
  description: string;
}

export interface IssueComment {
  id: number;
  body: string;
  user: string;
  createdSeq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  user: string;
  createdSeq: number;
  updatedSeq: number;
  comments: IssueComment[];
}

export interface PullRequest {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  user: string;
  createdSeq: number;
  updatedSeq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdSeq: number;
  /** Object id -> object, this repository's own object database. */
  objects: Map<string, GitObject>;
  /** Full ref name (refs/heads/main, refs/tags/v1) -> tip object id. */
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, PullRequest>;
  /** One counter for issues and pull requests, as GitHub numbers them. */
  nextNumber: number;
  nextCommentId: number;
}

/** 2025-01-01T00:00:00Z, the epoch every deterministic timestamp derives from. */
export const EPOCH_SEC = 1_735_689_600;

/** The login the trial's bearer token names (wire.ts buildScenario, fixed seed). */
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const VIEWER_ID = 1000;

export class Store {
  repos = new Map<string, Repo>();
  nextRepoId = 1;
  /** Deterministic clock: incremented once per created entity or commit. */
  seq = 0;

  key(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  /** The next deterministic timestamp, as epoch seconds. */
  tick(): number {
    this.seq += 1;
    return EPOCH_SEC + this.seq;
  }

  isoOf(epochSec: number): string {
    return new Date(epochSec * 1000).toISOString().replace('.000Z', 'Z');
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
    const id = this.nextRepoId;
    this.nextRepoId += 1;
    const repo: Repo = {
      id,
      name,
      owner,
      description,
      private: isPrivate,
      defaultBranch: 'main',
      createdSeq: this.seq + 1,
      objects: new Map(),
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      nextCommentId: 1,
    };
    this.seq += 1;
    this.repos.set(this.key(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) {
      if (repo.owner === VIEWER_LOGIN) out.push(repo);
    }
    out.sort((a, b) => a.id - b.id);
    return out;
  }
}
