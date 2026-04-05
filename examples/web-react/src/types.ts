// GoAgent SSE Event Types
export interface SSEEvent {
  type: string;
  text?: string;
  thinking?: string;
  tool_name?: string;
  tool_result?: string;
  error?: string;
  session_id?: string;
  request_id?: string;
  tool_input?: unknown;
  permission?: string;
  usage?: {
    input_tokens: number;
    output_tokens: number;
  };
}

export interface PermissionRequest {
  type: 'permission_request';
  request_id: string;
  session_id: string;
  tool_name: string;
  tool_input: unknown;
  permission: string;
}

export interface AskUserEvent {
  type: 'ask_user';
  request_id: string;
  question: string;
  session_id?: string;
}

export interface PlanConfirmEvent {
  type: 'plan_confirm';
  request_id: string;
  content: string;
  session_id?: string;
}

export interface InterruptEvent {
  type: 'interrupt';
  reason: string;
  session_id?: string;
}

export interface MetadataEvent {
  type: 'metadata';
  elapsed_ms: number;
}

export type GoAgentEvent =
  | SSEEvent
  | PermissionRequest
  | AskUserEvent
  | PlanConfirmEvent
  | InterruptEvent
  | MetadataEvent;

// Message for display
export interface Message {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  thinking?: string;
  toolName?: string;
  toolResult?: string;
  timestamp: Date;
}

// Task for display
export interface Task {
  id: string;
  subject: string;
  description?: string;
  status: 'pending' | 'in_progress' | 'completed';
  active_form?: string;
}
