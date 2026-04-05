import { useState } from 'react';
import { useGoAgent } from './hooks/useGoAgent';
import type { Message, PermissionRequest, Task } from './types';

// Icons
const SendIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <line x1="22" y1="2" x2="11" y2="13"></line>
    <polygon points="22 2 15 22 11 13 2 9 22 2"></polygon>
  </svg>
);

const UserIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"></path>
    <circle cx="12" cy="7" r="4"></circle>
  </svg>
);

const BotIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
    <rect x="3" y="11" width="18" height="10" rx="2"></rect>
    <circle cx="12" cy="5" r="2"></circle>
    <path d="M12 7v4"></path>
    <line x1="8" y1="16" x2="8" y2="16"></line>
    <line x1="16" y1="16" x2="16" y2="16"></line>
  </svg>
);

const PauseIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <rect x="6" y="4" width="4" height="16"></rect>
    <rect x="14" y="4" width="4" height="16"></rect>
  </svg>
);

const PlayIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <polygon points="5 3 19 12 5 21 5 3"></polygon>
  </svg>
);

const StopIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <rect x="3" y="3" width="18" height="18" rx="2" ry="2"></rect>
  </svg>
);

const CheckIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <polyline points="20 6 9 17 4 12"></polyline>
  </svg>
);

const ChevronDownIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <polyline points="6 9 12 15 18 9"></polyline>
  </svg>
);

const XIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <line x1="18" y1="6" x2="6" y2="18"></line>
    <line x1="6" y1="6" x2="18" y2="18"></line>
  </svg>
);

const RefreshIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <polyline points="23 4 23 10 17 10"></polyline>
    <path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"></path>
  </svg>
);

const PlanIcon = () => (
  <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
    <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"></path>
    <polyline points="14 2 14 8 20 8"></polyline>
    <line x1="16" y1="13" x2="8" y2="13"></line>
    <line x1="16" y1="17" x2="8" y2="17"></line>
    <polyline points="10 9 9 9 8 9"></polyline>
  </svg>
);

// Tool Call Message Component - displays tool calls as separate entries like CLI
function ToolCallMessage({ message }: { message: Message }) {
  return (
    <div className="flex bg-[#f7f7f8] py-5 px-4">
      <div className="max-w-3xl mx-auto w-full flex gap-4">
        <div className="flex-shrink-0 w-8 h-8 rounded-lg flex items-center justify-center bg-[#5436da] text-white">
          <BotIcon />
        </div>
        <div className="flex-1 min-w-0">
          {/* Tool name header */}
          <div className="flex items-center gap-2 mb-2">
            <span className="bg-blue-600 text-white text-xs px-2 py-1 rounded font-medium">
              {message.toolName}
            </span>
            <span className="text-xs text-gray-500">Tool Execution</span>
          </div>
          {/* Tool result */}
          {message.toolResult && (
            <div className="p-3 bg-gray-900 text-gray-100 border border-gray-700 rounded-lg text-xs whitespace-pre-wrap font-mono max-h-64 overflow-auto">
              {message.toolResult}
            </div>
          )}
          {!message.toolResult && (
            <div className="flex items-center gap-2 text-gray-400">
              <div className="w-4 h-4 border-2 border-gray-400 border-t-transparent rounded-full animate-spin"></div>
              <span className="text-xs">Executing...</span>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// Message Component
function ChatMessage({ message }: { message: Message }) {
  const [showThinking, setShowThinking] = useState(false);
  const isAssistant = message.role === 'assistant';

  // If this message is a tool call wrapper, render differently
  if (message.toolName && !message.content?.startsWith('[')) {
    return <ToolCallMessage message={message} />;
  }

  return (
    <div className={`flex ${isAssistant ? 'bg-[#f7f7f8]' : ''} py-5 px-4`}>
      <div className="max-w-3xl mx-auto w-full flex gap-4">
        <div className={`flex-shrink-0 w-8 h-8 rounded-lg flex items-center justify-center ${
          isAssistant ? 'bg-[#5436da] text-white' : 'bg-[#5436da] text-white'
        }`}>
          {isAssistant ? <BotIcon /> : <UserIcon />}
        </div>
        <div className="flex-1 min-w-0">
          <div className="text-[15px] leading-relaxed text-gray-800 whitespace-pre-wrap break-words">
            {message.content}
          </div>
          {message.thinking && (
            <div className="mt-2">
              <button
                onClick={() => setShowThinking(!showThinking)}
                className="flex items-center gap-1 text-xs text-gray-500 hover:text-gray-700 transition-colors"
              >
                <ChevronDownIcon />
                {showThinking ? 'Hide' : 'Show'} thinking
              </button>
              {showThinking && (
                <div className="mt-2 p-3 bg-amber-50 border border-amber-200 rounded-lg text-xs text-amber-800 whitespace-pre-wrap font-mono max-h-60 overflow-auto">
                  {message.thinking}
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// Task List Component
function TaskSidebar({ tasks, onRefresh }: { tasks: Task[]; onRefresh: () => void }) {
  const statusColors: Record<string, string> = {
    pending: 'bg-gray-400',
    in_progress: 'bg-blue-500',
    completed: 'bg-green-500',
  };

  return (
    <div className="w-72 bg-[#202123] text-white flex flex-col h-full">
      <div className="p-4 border-b border-gray-700 flex items-center justify-between">
        <h3 className="font-semibold text-sm">Tasks</h3>
        <button
          onClick={onRefresh}
          className="p-1.5 hover:bg-gray-700 rounded transition-colors"
          title="Refresh tasks"
        >
          <RefreshIcon />
        </button>
      </div>
      <div className="flex-1 overflow-y-auto p-2">
        {tasks.length === 0 ? (
          <p className="text-gray-400 text-sm p-2">No tasks</p>
        ) : (
          <ul className="space-y-1">
            {tasks.map((task) => (
              <li key={task.id} className="p-2 rounded hover:bg-gray-700 transition-colors cursor-pointer group">
                <div className="flex items-center gap-2">
                  <span className={`w-2 h-2 rounded-full flex-shrink-0 ${statusColors[task.status]}`}></span>
                  <span className="text-sm truncate flex-1">{task.subject}</span>
                  {task.status === 'completed' && (
                    <span className="text-green-400"><CheckIcon /></span>
                  )}
                </div>
                {task.active_form && (
                  <p className="text-xs text-gray-400 mt-1 truncate pl-4">{task.active_form}</p>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

// Plan Content Panel
function PlanPanel({ content, onClose }: { content: string; onClose: () => void }) {
  return (
    <div className="w-80 bg-[#202123] text-white flex flex-col h-full border-l border-gray-700">
      <div className="p-4 border-b border-gray-700 flex items-center justify-between">
        <h3 className="font-semibold text-sm flex items-center gap-2">
          <PlanIcon /> Plan Content
        </h3>
        <button
          onClick={onClose}
          className="p-1.5 hover:bg-gray-700 rounded transition-colors"
        >
          <XIcon />
        </button>
      </div>
      <div className="flex-1 overflow-y-auto p-4">
        <pre className="text-xs font-mono text-gray-300 whitespace-pre-wrap">
          {content}
        </pre>
      </div>
    </div>
  );
}

// Permission Dialog
function PermissionDialog({
  permission,
  onApprove,
  onDeny,
}: {
  permission: PermissionRequest;
  onApprove: (alwaysAllow: boolean) => void;
  onDeny: () => void;
}) {
  const [alwaysAllow, setAlwaysAllow] = useState(false);

  const formatInput = (input: unknown): string => {
    if (typeof input === 'string') return input;
    try {
      return JSON.stringify(input, null, 2);
    } catch {
      return String(input);
    }
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
      <div className="bg-white rounded-2xl shadow-2xl max-w-lg w-full overflow-hidden">
        <div className="bg-orange-500 px-6 py-4">
          <h2 className="text-white text-lg font-semibold flex items-center gap-2">
            <span className="text-xl">⚠️</span> Permission Required
          </h2>
        </div>
        <div className="p-6 space-y-4">
          <div className="flex items-center gap-3">
            <div className="bg-orange-100 text-orange-700 px-3 py-1.5 rounded-full text-sm font-bold">
              {permission.tool_name}
            </div>
            <span className="text-gray-500 text-sm capitalize">{permission.permission}</span>
          </div>
          <div>
            <label className="text-sm font-medium text-gray-700 mb-2 block">Tool Input</label>
            <pre className="bg-gray-900 text-gray-100 p-4 rounded-xl text-xs overflow-auto max-h-48 font-mono">
              {formatInput(permission.tool_input)}
            </pre>
          </div>
          <label className="flex items-center gap-2 text-sm text-gray-600 cursor-pointer">
            <input
              type="checkbox"
              checked={alwaysAllow}
              onChange={(e) => setAlwaysAllow(e.target.checked)}
              className="w-4 h-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
            />
            Always allow for this tool
          </label>
        </div>
        <div className="flex gap-3 p-4 bg-gray-50">
          <button
            onClick={() => onApprove(alwaysAllow)}
            className="flex-1 bg-green-600 text-white py-3 px-4 rounded-xl font-medium hover:bg-green-700 transition-colors"
          >
            Approve
          </button>
          <button
            onClick={onDeny}
            className="px-6 py-3 border border-gray-300 rounded-xl font-medium hover:bg-gray-100 transition-colors text-gray-700"
          >
            Deny
          </button>
        </div>
      </div>
    </div>
  );
}

// Ask User Dialog
function AskUserDialog({
  question,
  onRespond,
}: {
  question: string;
  onRespond: (answer: string) => void;
}) {
  const [answer, setAnswer] = useState('');

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (answer.trim()) {
      onRespond(answer.trim());
    }
  };

  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
      <div className="bg-white rounded-2xl shadow-2xl max-w-lg w-full overflow-hidden">
        <div className="bg-blue-500 px-6 py-4">
          <h2 className="text-white text-lg font-semibold flex items-center gap-2">
            <span>💬</span> Question
          </h2>
        </div>
        <form onSubmit={handleSubmit} className="p-6 space-y-4">
          <div className="bg-blue-50 border border-blue-200 rounded-xl p-4">
            <p className="text-gray-800 whitespace-pre-wrap">{question}</p>
          </div>
          <textarea
            value={answer}
            onChange={(e) => setAnswer(e.target.value)}
            placeholder="Type your answer..."
            className="w-full p-4 border border-gray-300 rounded-xl resize-none focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent"
            rows={4}
            autoFocus
          />
          <div className="flex justify-end">
            <button
              type="submit"
              disabled={!answer.trim()}
              className="bg-blue-500 text-white px-6 py-3 rounded-xl font-medium hover:bg-blue-600 transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
            >
              Submit
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

// Plan Confirm Dialog
function PlanConfirmDialog({
  content,
  onConfirm,
}: {
  content: string;
  onConfirm: (confirm: boolean) => void;
}) {
  return (
    <div className="fixed inset-0 bg-black/50 flex items-center justify-center z-50 p-4">
      <div className="bg-white rounded-2xl shadow-2xl max-w-2xl w-full overflow-hidden">
        <div className="bg-purple-500 px-6 py-4">
          <h2 className="text-white text-lg font-semibold flex items-center gap-2">
            <PlanIcon /> Plan Proposed
          </h2>
        </div>
        <div className="p-6">
          <div className="bg-gray-900 text-gray-100 p-4 rounded-xl text-sm font-mono whitespace-pre-wrap max-h-80 overflow-auto">
            {content}
          </div>
        </div>
        <div className="flex gap-3 p-4 bg-gray-50">
          <button
            onClick={() => onConfirm(true)}
            className="flex-1 bg-green-600 text-white py-3 px-4 rounded-xl font-medium hover:bg-green-700 transition-colors"
          >
            Approve Plan
          </button>
          <button
            onClick={() => onConfirm(false)}
            className="px-6 py-3 border border-gray-300 rounded-xl font-medium hover:bg-gray-100 transition-colors text-gray-700"
          >
            Reject
          </button>
        </div>
      </div>
    </div>
  );
}

// Main App
export default function App() {
  const [BASE_URL] = useState('http://localhost:8080');
  const [showSidebar, setShowSidebar] = useState(true);
  const [showPlanPanel, setShowPlanPanel] = useState(false);
  const [input, setInput] = useState('');
  const [planConfirmData, setPlanConfirmData] = useState<{ content: string } | null>(null);

  const {
    messages,
    tasks,
    planActive,
    planContent,
    pendingPermissions,
    pendingAskUser,
    isConnected,
    isPaused,
    error,
    sendMessage,
    pause,
    resume,
    approvePermission,
    denyPermission,
    respondToAskUser,
    confirmPlan,
    interrupt,
    clearError,
    fetchTasks,
  } = useGoAgent({ baseUrl: BASE_URL });

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (input.trim() && !isConnected) {
      sendMessage(input.trim());
      setInput('');
    }
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      handleSubmit(e);
    }
  };

  const handlePlanConfirm = (confirm: boolean) => {
    if (planConfirmData) {
      confirmPlan(confirm);
      setPlanConfirmData(null);
    }
  };

  return (
    <div className="h-screen flex bg-white">
      {/* Sidebar */}
      {showSidebar && <TaskSidebar tasks={tasks} onRefresh={fetchTasks} />}

      {/* Main Chat Area */}
      <div className="flex-1 flex flex-col min-w-0">
        {/* Header */}
        <header className="h-14 border-b border-gray-200 flex items-center justify-between px-4 bg-white">
          <div className="flex items-center gap-3">
            <button
              onClick={() => setShowSidebar(!showSidebar)}
              className="p-2 hover:bg-gray-100 rounded-lg transition-colors"
            >
              <svg xmlns="http://www.w3.org/2000/svg" width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                <rect x="3" y="3" width="18" height="18" rx="2"></rect>
                <line x1="9" y1="3" x2="9" y2="21"></line>
              </svg>
            </button>
            <span className="font-semibold text-gray-800">GoAgent</span>
            {planActive && (
              <button
                onClick={() => setShowPlanPanel(!showPlanPanel)}
                className="flex items-center gap-1 text-xs bg-purple-100 text-purple-700 px-2 py-0.5 rounded-full hover:bg-purple-200 transition-colors"
              >
                <PlanIcon /> Plan Mode
              </button>
            )}
          </div>
          <div className="flex items-center gap-2">
            {/* Pause/Resume Button */}
            {isConnected && (
              <>
                {isPaused ? (
                  <button
                    onClick={resume}
                    className="flex items-center gap-1.5 px-3 py-1.5 bg-green-600 text-white rounded-lg hover:bg-green-700 transition-colors text-sm"
                    title="Resume"
                  >
                    <PlayIcon /> Resume
                  </button>
                ) : (
                  <button
                    onClick={pause}
                    className="flex items-center gap-1.5 px-3 py-1.5 bg-orange-500 text-white rounded-lg hover:bg-orange-600 transition-colors text-sm"
                    title="Pause"
                  >
                    <PauseIcon /> Pause
                  </button>
                )}
                <button
                  onClick={interrupt}
                  className="flex items-center gap-1.5 px-3 py-1.5 bg-red-500 text-white rounded-lg hover:bg-red-600 transition-colors text-sm"
                  title="Stop"
                >
                  <StopIcon /> Stop
                </button>
              </>
            )}
            {isConnected ? (
              <span className="flex items-center gap-2 text-sm text-green-600">
                <span className="w-2 h-2 bg-green-500 rounded-full animate-pulse"></span>
                Running
              </span>
            ) : (
              <span className="text-sm text-gray-400">Ready</span>
            )}
          </div>
        </header>

        {/* Messages */}
        <div className="flex-1 overflow-y-auto">
          {messages.length === 0 ? (
            <div className="h-full flex flex-col items-center justify-center text-gray-500">
              <div className="w-16 h-16 bg-[#5436da] rounded-2xl flex items-center justify-center mb-4 text-white">
                <BotIcon />
              </div>
              <h2 className="text-xl font-semibold text-gray-800 mb-2">GoAgent Assistant</h2>
              <p className="text-sm">Start a conversation by typing a message below</p>
            </div>
          ) : (
            <div className="max-w-3xl mx-auto">
              {messages.map((msg) => (
                <ChatMessage key={msg.id} message={msg} />
              ))}
            </div>
          )}
        </div>

        {/* Input Area */}
        <div className="border-t border-gray-200 p-4 bg-white">
          <form onSubmit={handleSubmit} className="max-w-3xl mx-auto">
            <div className="relative flex items-end gap-3">
              <textarea
                value={input}
                onChange={(e) => setInput(e.target.value)}
                onKeyDown={handleKeyDown}
                placeholder="Type a message..."
                disabled={isConnected}
                rows={1}
                className="flex-1 p-4 pr-12 border border-gray-300 rounded-2xl resize-none focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent disabled:bg-gray-100"
                style={{ minHeight: '56px', maxHeight: '200px' }}
              />
              <button
                type="submit"
                disabled={!input.trim() || isConnected}
                className="absolute right-3 bottom-3 p-2.5 bg-[#5436da] text-white rounded-xl disabled:opacity-50 disabled:cursor-not-allowed hover:bg-[#4336ca] transition-colors"
              >
                <SendIcon />
              </button>
            </div>
            <p className="text-xs text-gray-400 mt-2 text-center">
              Press Enter to send, Shift+Enter for new line
            </p>
          </form>
        </div>
      </div>

      {/* Plan Panel */}
      {showPlanPanel && planContent && (
        <PlanPanel content={planContent} onClose={() => setShowPlanPanel(false)} />
      )}

      {/* Permission Dialog */}
      {pendingPermissions.length > 0 && (
        <PermissionDialog
          permission={pendingPermissions[0]}
          onApprove={(alwaysAllow) => approvePermission(pendingPermissions[0].request_id, alwaysAllow)}
          onDeny={() => denyPermission(pendingPermissions[0].request_id)}
        />
      )}

      {/* Ask User Dialog */}
      {pendingAskUser && (
        <AskUserDialog
          question={pendingAskUser.question}
          onRespond={respondToAskUser}
        />
      )}

      {/* Plan Confirm Dialog */}
      {planConfirmData && (
        <PlanConfirmDialog
          content={planConfirmData.content}
          onConfirm={handlePlanConfirm}
        />
      )}

      {/* Error Toast */}
      {error && (
        <div className="fixed bottom-4 right-4 bg-red-500 text-white px-6 py-3 rounded-xl shadow-lg flex items-center gap-3">
          <span>{error}</span>
          <button onClick={clearError} className="p-1 hover:bg-red-600 rounded">
            <XIcon />
          </button>
        </div>
      )}
    </div>
  );
}
