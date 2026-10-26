// Trim a file name without changing its case.
export function Normalize(value: string): string {
  return value.trim();
}

export class Parser {
  Parse(value: string): string { return Normalize(value); }
}
