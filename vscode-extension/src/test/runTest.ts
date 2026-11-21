import * as fs from "fs";
import * as path from "path";
import { runTests } from "@vscode/test-electron";

/**
 * 探测系统已安装的 VS Code 可执行文件路径。
 */
function findSystemCode(): string | undefined {
  if (process.env.VSCODE_PATH && fs.existsSync(process.env.VSCODE_PATH)) {
    return process.env.VSCODE_PATH;
  }
  const binaryName = process.platform === "win32" ? "code.cmd" : "code";
  const pathDirs = (process.env.PATH || "").split(path.delimiter).filter(Boolean);
  for (const dir of pathDirs) {
    const candidate = path.join(dir, binaryName);
    try {
      if (fs.existsSync(candidate) && fs.statSync(candidate).isFile()) {
        return candidate;
      }
    } catch {
      // 忽略无权限或不存在的目录
    }
  }
  return undefined;
}

async function main() {
  try {
    const extensionDevelopmentPath = path.resolve(__dirname, "../../");
    const extensionTestsPath = path.resolve(__dirname, "./suite/index");
    const testWorkspace = path.resolve(
      __dirname,
      "../../../testdata/fixtures/projects/npm-context"
    );

    const vscodeExecutablePath = findSystemCode();

    // 运行测试并启动 Extension Development Host
    await runTests({
      ...(vscodeExecutablePath ? { vscodeExecutablePath } : {}),
      extensionDevelopmentPath,
      extensionTestsPath,
      launchArgs: [testWorkspace, "--disable-extensions"],
    });
  } catch (err) {
    console.error("Failed to run tests:", err);
    process.exit(1);
  }
}

main();
