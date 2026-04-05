import { useState, useCallback, useRef, useEffect } from 'react';
import type { GoAgentEvent, Message, PermissionRequest, Task } from '../types';

interface UseGoAgentOptions {
  baseUrl: string;
  sessionId?: string;
}

interface UseGoAgentReturn {
  messages: Message[];
  tasks: Task[];
  planActive: boolean;
  planContent: string;
  pendingPermissions: PermissionRequest[];
  pendingAskUser: { request_id: string; question: string } | null;
  isConnected: boolean;
  isPaused: boolean;
  error: string | null;
  sendMessage: (message: string) => void;
  pause: () => void;
  resume: () => void;
  approvePermission: (requestId: string, alwaysAllow?: boolean) => void;
  denyPermission: (requestId: string, reason?: string) => void;
  respondToAskUser: (answer: string) => void;
  confirmPlan: (confirm: boolean) => void;
  interrupt: () => void;
  clearError: () => void;
  fetchTasks: () => Promise<void>;
}

export function useGoAgent({
  baseUrl,
  sessionId: initialSessionId,
}: UseGoAgentOptions): UseGoAgentReturn {
  const [messages, setMessages] = useState<Message[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [planActive, setPlanActive] = useState(false);
  const [planContent, setPlanContent] = useState('');
  const [pendingPermissions, setPendingPermissions] = useState<PermissionRequest[]>([]);
  const [pendingAskUser, setPendingAskUser] = useState<{ request_id: string; question: string } | null>(null);
  const [isConnected, setIsConnected] = useState(false);
  const [isPaused, setIsPaused] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const abortControllerRef = useRef<AbortController | null>(null);
  const sessionIdRef = useRef(initialSessionId || '');

  // Fetch tasks
  const fetchTasks = useCallback(async () => {
    try {
      const response = await fetch(`${baseUrl}/tasks`);
      if (response.ok) {
        const data = await response.json();
        setTasks(data);
      }
    } catch (err) {
      console.error('Failed to fetch tasks:', err);
    }
  }, [baseUrl]);

  // Fetch plan status
  const fetchPlan = useCallback(async () => {
    try {
      const response = await fetch(`${baseUrl}/plan`);
      if (response.ok) {
        const data = await response.json();
        setPlanActive(data.active);
        setPlanContent(data.content || '');
      }
    } catch (err) {
      console.error('Failed to fetch plan:', err);
    }
  }, [baseUrl]);

  // Send message using fetch + ReadableStream (SSE over POST)
  const sendMessage = useCallback(
    async (message: string) => {
      // Cancel any existing request
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }

      const sessionId = sessionIdRef.current || `sess-${Date.now()}`;
      sessionIdRef.current = sessionId;
      abortControllerRef.current = new AbortController();

      setIsConnected(true);
      setError(null);

      try {
        const response = await fetch(`${baseUrl}/chat`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            message,
            session_id: sessionId,
          }),
          signal: abortControllerRef.current.signal,
        });

        if (!response.ok) {
          throw new Error(`HTTP error: ${response.status}`);
        }

        const reader = response.body?.getReader();
        if (!reader) {
          throw new Error('No response body');
        }

        const decoder = new TextDecoder();
        let buffer = '';

        // Process SSE stream
        while (true) {
          const { done, value } = await reader.read();
          if (done) break;

          buffer += decoder.decode(value, { stream: true });
          const lines = buffer.split('\n');
          buffer = lines.pop() || '';

          for (const line of lines) {
            if (line.startsWith('data: ')) {
              try {
                const data: GoAgentEvent = JSON.parse(line.slice(6));

                // Handle different event types
                switch (data.type) {
                  case 'text_delta':
                    setMessages((prev) => {
                      const lastMsg = prev[prev.length - 1];
                      if (lastMsg && lastMsg.role === 'assistant') {
                        return [
                          ...prev.slice(0, -1),
                          { ...lastMsg, content: lastMsg.content + (data.text || '') },
                        ];
                      }
                      return [
                        ...prev,
                        {
                          id: `msg-${Date.now()}`,
                          role: 'assistant',
                          content: data.text || '',
                          timestamp: new Date(),
                        },
                      ];
                    });
                    break;

                  case 'thinking':
                    setMessages((prev) => {
                      const lastMsg = prev[prev.length - 1];
                      if (lastMsg && lastMsg.role === 'assistant') {
                        return [
                          ...prev.slice(0, -1),
                          { ...lastMsg, thinking: (lastMsg.thinking || '') + (data.thinking || '') },
                        ];
                      }
                      return prev;
                    });
                    break;

                  case 'tool_start':
                    setMessages((prev) => [
                      ...prev,
                      {
                        id: `msg-${Date.now()}`,
                        role: 'assistant',
                        content: '',
                        toolName: data.tool_name,
                        timestamp: new Date(),
                      },
                    ]);
                    break;

                  case 'tool_done':
                    setMessages((prev) => {
                      const lastMsg = prev[prev.length - 1];
                      if (lastMsg && lastMsg.toolName) {
                        return [
                          ...prev.slice(0, -1),
                          { ...lastMsg, toolResult: data.tool_result },
                        ];
                      }
                      return prev;
                    });
                    // Refresh tasks after tool execution
                    fetchTasks();
                    break;

                  case 'need_approval':
                  case 'permission_request':
                    setPendingPermissions((prev) => [
                      ...prev,
                      data as PermissionRequest,
                    ]);
                    break;

                  case 'ask_user':
                    setPendingAskUser({ request_id: (data as any).request_id || '', question: (data as any).question || '' });
                    break;

                  case 'plan_confirm':
                    // Plan needs confirmation - store request_id for confirm
                    (window as any).pendingPlanConfirm = (data as any).request_id;
                    fetchPlan();
                    break;

                  case 'interrupt':
                    setIsPaused(true);
                    break;

                  case 'done':
                  case 'metadata':
                    setIsConnected(false);
                    fetchTasks();
                    fetchPlan();
                    break;

                  case 'error':
                    setError(data.error || 'Unknown error');
                    setIsConnected(false);
                    break;

                  case 'compaction':
                    // Context was compacted
                    console.log('Compaction happened');
                    break;
                }
              } catch (err) {
                console.error('Failed to parse SSE event:', err);
              }
            }
          }
        }
      } catch (err) {
        if ((err as Error).name === 'AbortError') {
          console.log('Request was cancelled');
        } else {
          setError((err as Error).message);
        }
        setIsConnected(false);
      } finally {
        setIsConnected(false);
      }
    },
    [baseUrl, fetchTasks, fetchPlan]
  );

  // Pause execution
  const pause = useCallback(async () => {
    try {
      await fetch(`${baseUrl}/interrupt`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sessionIdRef.current }),
      });
      setIsPaused(true);
    } catch (err) {
      setError('Failed to pause');
    }
  }, [baseUrl]);

  // Resume execution
  const resume = useCallback(() => {
    // Resume is done by sending a new message
    setIsPaused(false);
  }, []);

  // Approve permission
  const approvePermission = useCallback(
    async (requestId: string, alwaysAllow = false) => {
      try {
        await fetch(`${baseUrl}/approve`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            request_id: requestId,
            session_id: sessionIdRef.current,
            allow: true,
            always_allow: alwaysAllow,
          }),
        });
        setPendingPermissions((prev) =>
          prev.filter((p) => p.request_id !== requestId)
        );
      } catch (err) {
        setError('Failed to approve permission');
      }
    },
    [baseUrl]
  );

  // Deny permission
  const denyPermission = useCallback(
    async (requestId: string, reason = '') => {
      try {
        await fetch(`${baseUrl}/approve`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            request_id: requestId,
            session_id: sessionIdRef.current,
            allow: false,
            reason,
          }),
        });
        setPendingPermissions((prev) =>
          prev.filter((p) => p.request_id !== requestId)
        );
      } catch (err) {
        setError('Failed to deny permission');
      }
    },
    [baseUrl]
  );

  // Respond to AskUser
  const respondToAskUser = useCallback(
    async (answer: string) => {
      try {
        await fetch(`${baseUrl}/askuser`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            request_id: pendingAskUser?.request_id,
            answer,
          }),
        });
        setPendingAskUser(null);
      } catch (err) {
        setError('Failed to respond to question');
      }
    },
    [baseUrl, pendingAskUser]
  );

  // Confirm plan
  const confirmPlan = useCallback(
    async (confirm: boolean) => {
      try {
        const requestId = (window as any).pendingPlanConfirm;
        await fetch(`${baseUrl}/plan/confirm`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            request_id: requestId,
            confirm,
          }),
        });
        delete (window as any).pendingPlanConfirm;
        fetchPlan();
      } catch (err) {
        setError('Failed to confirm plan');
      }
    },
    [baseUrl, fetchPlan]
  );

  // Interrupt execution
  const interrupt = useCallback(async () => {
    try {
      await fetch(`${baseUrl}/interrupt`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: sessionIdRef.current }),
      });
      setIsConnected(false);
      setIsPaused(true);
    } catch (err) {
      setError('Failed to interrupt');
    }
  }, [baseUrl]);

  // Clear error
  const clearError = useCallback(() => {
    setError(null);
  }, []);

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      if (abortControllerRef.current) {
        abortControllerRef.current.abort();
      }
    };
  }, []);

  // Initial fetch
  useEffect(() => {
    fetchTasks();
    fetchPlan();
  }, [fetchTasks, fetchPlan]);

  return {
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
  };
}
