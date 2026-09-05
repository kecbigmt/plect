// Hand-written, not generated: a thin openapi-fetch client bound to the
// generated `paths` type. Regenerate `generated/typescript/schema.d.ts`
// after editing the TypeSpec source (see README.md); this file only needs
// updating if the base URL or fetch options themselves change.
import createClient from "openapi-fetch";
import type { paths } from "./generated/typescript/schema.js";

export function createPlectureWebApiClient(baseUrl: string) {
  return createClient<paths>({ baseUrl });
}
