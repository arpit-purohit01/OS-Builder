// The spec store is the builder's single source of truth. Every edit is a
// command {patch, inverse, label}; undo/redo replay inverses (cap 200).
import { useSyncExternalStore } from "react";
import { apply, diff, setPath, type Op } from "../lib/jsonpatch";

export type Spec = Record<string, unknown>;
interface Command { patch: Op[]; inverse: Op[]; label: string; at: number }

const CAP = 200;

export class SpecStore {
  private doc: Spec;
  private undoStack: Command[] = [];
  private redoStack: Command[] = [];
  private subs = new Set<() => void>();
  private version = 0;
  saved: Spec;

  constructor(doc: Spec) {
    this.doc = doc;
    this.saved = doc;
  }

  get = () => this.doc;
  getVersion = () => this.version;
  subscribe = (f: () => void) => {
    this.subs.add(f);
    return () => this.subs.delete(f);
  };
  private emit() {
    this.version++;
    this.subs.forEach((f) => f());
  }

  // commit applies a whole new document as one command (coalescing rapid edits
  // to the same field, e.g. typing, within 800 ms).
  commit(next: Spec, label: string) {
    const patch = diff(this.doc, next);
    if (patch.length === 0) return;
    const inverse = diff(next, this.doc);
    const top = this.undoStack[this.undoStack.length - 1];
    const now = Date.now();
    if (top && top.label === label && now - top.at < 800) {
      // keep the first inverse: undo restores the state before the first keystroke
      top.patch = diff(apply(next, top.inverse), next);
      top.at = now;
    } else {
      this.undoStack.push({ patch, inverse, label, at: now });
      if (this.undoStack.length > CAP) this.undoStack.shift();
    }
    this.redoStack = [];
    this.doc = next;
    this.emit();
  }

  set(ptr: string, value: unknown, label = ptr) {
    this.commit(setPath(this.doc, ptr, value), label);
  }

  applyPatch(ops: Op[], label: string) {
    this.commit(apply(this.doc, ops), label);
  }

  undo() {
    const c = this.undoStack.pop();
    if (!c) return;
    const before = apply(this.doc, c.inverse);
    this.redoStack.push({ ...c, patch: diff(before, this.doc) });
    this.doc = before;
    this.emit();
  }

  redo() {
    const c = this.redoStack.pop();
    if (!c) return;
    const next = apply(this.doc, c.patch);
    this.undoStack.push({ ...c, inverse: diff(next, this.doc) });
    this.doc = next;
    this.emit();
  }

  reset(doc: Spec) {
    this.doc = doc;
    this.saved = doc;
    this.undoStack = [];
    this.redoStack = [];
    this.emit();
  }

  markSaved() {
    this.saved = this.doc;
    this.emit();
  }

  get dirty() {
    return diff(this.saved, this.doc).length > 0;
  }
  get canUndo() {
    return this.undoStack.length > 0;
  }
  get canRedo() {
    return this.redoStack.length > 0;
  }
  get history() {
    return this.undoStack.map((c) => c.label);
  }
}

export function useSpec(store: SpecStore): Spec {
  useSyncExternalStore(store.subscribe, store.getVersion);
  return store.get();
}
