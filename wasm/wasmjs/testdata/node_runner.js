// Runs a comparison script in Node: node node_runner.js builder prelude script
const fs = require("fs"), vm = require("vm");
for (const f of process.argv.slice(2)) vm.runInThisContext(fs.readFileSync(f, "utf8"), { filename: f });
vm.runInThisContext("chain").then(() => process.stdout.write(vm.runInThisContext("out").join("\n") + "\n"));
