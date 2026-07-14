import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const mobileDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const fixedGeneratedPaths = [
  path.join(mobileDir, "android"),
  path.join(mobileDir, ".expo"),
  path.join(mobileDir, "dist"),
];

for (const generatedPath of fixedGeneratedPaths) {
  fs.rmSync(generatedPath, { force: true, recursive: true });
}
console.log("Removed fixed Android generated/build output paths");
