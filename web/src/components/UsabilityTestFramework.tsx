import { useState } from "react";

interface TestSession {
  id: string;
  participantId: string;
  projectId: string;
  startTime: string;
  endTime?: string;
  tasks: TaskResult[];
  overallRating?: number;
  notes: string;
}

interface TaskResult {
  taskId: string;
  taskName: string;
  success: boolean;
  timeSeconds: number;
  errors: string[];
  difficulty: 1 | 2 | 3 | 4 | 5;
  comments: string;
}

interface TestScenario {
  id: string;
  name: string;
  description: string;
  expectedTimeSeconds: number;
  steps: string[];
  successCriteria: string[];
}

const TEST_SCENARIOS: TestScenario[] = [
  {
    id: "create-kiosk",
    name: "Create a Kiosk OS",
    description: "Build a single-app kiosk OS for a lobby display",
    expectedTimeSeconds: 300,
    steps: [
      "Open the builder and select 'Kiosk' template",
      "Configure the kiosk URL and idle reset time",
      "Set security level to 'hardened'",
      "Choose TPM2 disk encryption",
      "Save and build the OS",
      "Download the ISO image",
    ],
    successCriteria: [
      "Template selected correctly",
      "Kiosk URL configured",
      "Security settings applied",
      "Build completes successfully",
      "ISO downloaded",
    ],
  },
  {
    id: "office-laptop",
    name: "Create an Office Laptop OS",
    description: "Build an office laptop OS with SSO, app store, and managed updates",
    expectedTimeSeconds: 600,
    steps: [
      "Open the builder and select 'Office laptop' template",
      "Configure SSO/OIDC identity provider",
      "Enable app store and select apps (Firefox, LibreOffice)",
      "Configure disk encryption with TPM2+PIN",
      "Set up fleet management enrollment",
      "Customize branding (OS name, colors, logo)",
      "Save and build the OS",
    ],
    successCriteria: [
      "Template selected correctly",
      "SSO configured with issuer and client ID",
      "Apps selected and added",
      "Encryption configured with PIN",
      "Fleet enrollment enabled",
      "Branding applied",
      "Build completes successfully",
    ],
  },
  {
    id: "school-lab",
    name: "Create a School Lab OS",
    description: "Build a school computer lab OS with printing, locked-down access, and fleet management",
    expectedTimeSeconds: 600,
    steps: [
      "Open the builder and select 'School lab' template",
      "Configure printing system (CUPS)",
      "Set security level to 'balanced'",
      "Configure USB storage as read-only",
      "Set up fleet management with device group",
      "Disable admin access (locked root model)",
      "Save and build the OS",
    ],
    successCriteria: [
      "Template selected correctly",
      "Printing configured",
      "USB set to read-only",
      "Fleet enrollment configured",
      "Root access locked",
      "Build completes successfully",
    ],
  },
  {
    id: "custom-layout",
    name: "Create Custom Desktop Layout",
    description: "Design a custom panel layout with favorite apps",
    expectedTimeSeconds: 180,
    steps: [
      "Open the Layout Editor",
      "Add a new panel at the bottom",
      "Configure as dock style",
      "Add favorite apps to the dock",
      "Set panel size and autohide",
      "Save and apply layout",
    ],
    successCriteria: [
      "Layout editor opened",
      "Panel added and configured",
      "Favorite apps added to dock",
      "Layout saved and applied",
    ],
  },
  {
    id: "dual-boot-install",
    name: "Install Alongside Windows",
    description: "Install the OS alongside Windows using the installer",
    expectedTimeSeconds: 1200,
    steps: [
      "Boot from USB installer media",
      "Select 'Install alongside Windows'",
      "Confirm BitLocker recovery key availability",
      "Select target disk partition",
      "Configure user account",
      "Complete installation and reboot",
      "Verify dual-boot menu appears",
    ],
    successCriteria: [
      "Installer boots correctly",
      'Alongside Windows option selected',
      "BitLocker recovery key confirmed",
      "Installation completes",
      "Dual-boot menu works",
      "Both OSes boot successfully",
    ],
  },
];

interface UsabilityTestFrameworkProps {
  projectId: string;
  onSessionComplete: (session: TestSession) => void;
}

export default function UsabilityTestFramework({ projectId, onSessionComplete }: UsabilityTestFrameworkProps) {
  const [currentStep, setCurrentStep] = useState(0);
  const [currentTaskIndex, setCurrentTaskIndex] = useState(0);
  const [session, setSession] = useState<TestSession>({
    id: `test-${Date.now()}`,
    participantId: `p-${Date.now()}`,
    projectId,
    startTime: new Date().toISOString(),
    tasks: [],
    notes: "",
  });
  const [taskStartTime, setTaskStartTime] = useState<number>(Date.now());
  const [taskErrors, setTaskErrors] = useState<string[]>([]);
  const [taskDifficulty, setTaskDifficulty] = useState<1 | 2 | 3 | 4 | 5>(3);
  const [taskComments, setTaskComments] = useState("");
  const [participantId] = useState("");

  const currentScenario = TEST_SCENARIOS[currentStep];
  const currentTask = currentScenario?.steps[currentTaskIndex];

  const handleTaskComplete = (success: boolean) => {
    const timeSeconds = Math.floor((Date.now() - taskStartTime) / 1000);
    const taskResult: TaskResult = {
      taskId: `${currentScenario?.id}-${currentTaskIndex}`,
      taskName: currentTask || "Unknown",
      success,
      timeSeconds,
      errors: [...taskErrors],
      difficulty: taskDifficulty,
      comments: taskComments,
    };

    setSession(prev => ({
      ...prev,
      tasks: [...prev.tasks, taskResult],
    }));

    // Move to next task or scenario
    if (currentScenario && currentTaskIndex < currentScenario.steps.length - 1) {
      setCurrentTaskIndex(prev => prev + 1);
      setTaskStartTime(Date.now());
      setTaskErrors([]);
      setTaskDifficulty(3);
      setTaskComments("");
    } else if (currentStep < TEST_SCENARIOS.length - 1) {
      setCurrentStep(prev => prev + 1);
      setCurrentTaskIndex(0);
      setTaskStartTime(Date.now());
      setTaskErrors([]);
      setTaskDifficulty(3);
      setTaskComments("");
    } else {
      // Test complete
      setSession(prev => ({
        ...prev,
        endTime: new Date().toISOString(),
      }));
    }
  };

  const recordError = (error: string) => {
    setTaskErrors(prev => [...prev, error]);
  };

  const skipTask = () => {
    handleTaskComplete(false);
  };

  const finalizeSession = () => {
    const completedSession: TestSession = {
      ...session,
      participantId: participantId || `p-${Date.now()}`,
      endTime: new Date().toISOString(),
    };
    onSessionComplete(completedSession);
  };

  if (!currentScenario) {
    return (
      <div className="card stack" style={{ padding: 32, textAlign: "center" }}>
        <h2>Usability Test Complete</h2>
        <p className="muted">All scenarios completed. Click below to save the session.</p>
        <button className="btn primary" onClick={finalizeSession}>Save Session</button>
      </div>
    );
  }

  return (
    <div className="card stack" style={{ gap: 24 }}>
      <div className="pagehead">
        <div>
          <div className="small muted">Usability Test Framework</div>
          <h1>{currentScenario.name}</h1>
          <p className="muted">{currentScenario.description}</p>
        </div>
        <div className="toolbar">
          <span className="badge">{currentStep + 1} of {TEST_SCENARIOS.length}</span>
          <span className="badge info">Task {currentTaskIndex + 1} of {currentScenario.steps.length}</span>
          <span className="badge {session.tasks.filter(t => t.success).length > 0 ? 'ok' : 'muted'}">
            {session.tasks.filter(t => t.success).length} / {session.tasks.length} passed
          </span>
        </div>
      </div>

      <div className="card stack" style={{ gap: 16 }}>
        <div className="row spread">
          <h3 style={{ margin: 0 }}>Current Task: {currentTask}</h3>
          <span className="badge info">{Math.floor((Date.now() - taskStartTime) / 1000)}s elapsed</span>
        </div>

        <div className="stack" style={{ gap: 12 }}>
          <div className="small muted">
            <strong>Scenario:</strong> {currentScenario.description}
          </div>
          <div className="stack" style={{ gap: 8 }}>
            {currentScenario.steps.map((step, i) => (
              <div
                key={i}
                className={`row spread small ${i === currentTaskIndex ? "primary" : i < currentTaskIndex ? "ok" : "muted"}`}
                style={{ padding: "8px 12px", borderRadius: 4, background: i === currentTaskIndex ? "var(--primary-soft)" : "transparent" }}
              >
                <span>{i + 1}. {step}</span>
                {i < currentTaskIndex && <span className="badge ok">✓</span>}
                {i === currentTaskIndex && <span className="badge">Current</span>}
              </div>
            ))}
          </div>
        </div>

        <fieldset className="card stack" style={{ gap: 12, padding: 16 }}>
          <legend style={{ fontWeight: 600 }}>Task Assessment</legend>

          <div className="row spread">
            <label className="row small" style={{ gap: 8 }}>
              <input type="checkbox" checked={true} disabled /> Task completed
            </label>
            <label className="row small">
              <input type="checkbox" onChange={() => recordError("User got stuck")} /> User got stuck
            </label>
            <label className="row small">
              <input type="checkbox" onChange={() => recordError("UI confusing")} /> UI confusing
            </label>
            <label className="row small">
              <input type="checkbox" onChange={() => recordError("Wrong path taken")} /> Wrong path
            </label>
          </div>

          <div className="row spread">
            <label className="field" style={{ width: 200 }}>Difficulty (1-5)
              <select value={taskDifficulty} onChange={(e) => setTaskDifficulty(Number(e.target.value) as 1 | 2 | 3 | 4 | 5)}>
                <option value={1}>1 - Very Easy</option>
                <option value={2}>2 - Easy</option>
                <option value={3}>3 - Moderate</option>
                <option value={4}>4 - Hard</option>
                <option value={5}>5 - Very Hard</option>
              </select>
            </label>
          </div>

          <label className="field">
            Comments
            <textarea
              value={taskComments}
              onChange={(e) => setTaskComments(e.target.value)}
              placeholder="Any observations, quotes, or notes..."
              rows={3}
            />
          </label>
        </fieldset>

        <div className="row" style={{ gap: 8, justifyContent: "flex-end" }}>
          <button className="ghost" onClick={skipTask}>Skip Task</button>
          <button className="primary" onClick={() => handleTaskComplete(true)}>
            {currentTaskIndex === (currentScenario?.steps.length ?? 1) - 1 && currentStep === TEST_SCENARIOS.length - 1
              ? "Complete Test"
              : "Complete Task"}
          </button>
        </div>
      </div>

      {session.tasks.length > 0 && (
        <details className="card">
          <summary style={{ cursor: "pointer", fontWeight: 600 }}>Session Summary ({session.tasks.length} tasks)</summary>
          <div className="stack" style={{ gap: 12, paddingTop: 12 }}>
            <div className="row spread">
              <span>Total time: {Math.floor((Date.now() - new Date(session.startTime).getTime()) / 1000)}s</span>
              <span className="badge ok">{session.tasks.filter(t => t.success).length} passed</span>
              <span className="badge warn">{session.tasks.filter(t => !t.success).length} failed</span>
              <span className="badge info">Avg difficulty: {(session.tasks.reduce((s, t) => s + t.difficulty, 0) / session.tasks.length).toFixed(1)}</span>
            </div>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 12 }}>
              <thead>
                <tr style={{ borderBottom: "1px solid var(--line)", textAlign: "left" }}>
                  <th style={{ padding: "4px 8px" }}>Task</th>
                  <th style={{ padding: "4px 8px" }}>Status</th>
                  <th style={{ padding: "4px 8px" }}>Time</th>
                  <th style={{ padding: "4px 8px" }}>Difficulty</th>
                  <th style={{ padding: "4px 8px" }}>Errors</th>
                </tr>
              </thead>
              <tbody>
                {session.tasks.map((t, i) => (
                  <tr key={i} style={{ borderBottom: "1px solid var(--line)" }}>
                    <td style={{ padding: "4px 8px" }}>{t.taskName}</td>
                    <td style={{ padding: "4px 8px" }}>{t.success ? <span className="badge ok">✓</span> : <span className="badge bad">✗</span>}</td>
                    <td style={{ padding: "4px 8px" }}>{t.timeSeconds}s</td>
                    <td style={{ padding: "4px 8px" }}>{t.difficulty}/5</td>
                    <td style={{ padding: "4px 8px" }} className="small">{t.errors.join(", ") || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
    </div>
  );
}

export { TEST_SCENARIOS };
export type { TestSession, TaskResult, TestScenario };