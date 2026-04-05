import { useState, useRef, useEffect } from 'react';
import type { Message } from '../types';

interface ChatWindowProps {
  messages: Message[];
  isConnected: boolean;
  onSendMessage: (message: string) => void;
}

export function ChatWindow({ messages, isConnected, onSendMessage }: ChatWindowProps) {
  const [input, setInput] = useState('');
  const messagesEndRef = useRef<HTMLDivElement>(null);

  const scrollToBottom = () => {
    messagesEndRef.current?.scrollIntoView({ behavior: 'smooth' });
  };

  useEffect(() => {
    scrollToBottom();
  }, [messages]);

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (input.trim() && !isConnected) {
      onSendMessage(input.trim());
      setInput('');
    }
  };

  return (
    <div className="chat-window">
      <div className="messages-container">
        {messages.map((msg) => (
          <div key={msg.id} className={`message message-${msg.role}`}>
            <div className="message-header">
              <span className="message-role">{msg.role === 'user' ? 'You' : 'Assistant'}</span>
              <span className="message-time">
                {msg.timestamp.toLocaleTimeString()}
              </span>
            </div>
            {msg.thinking && (
              <div className="message-thinking">
                <details>
                  <summary>Thinking</summary>
                  <pre>{msg.thinking}</pre>
                </details>
              </div>
            )}
            <div className="message-content">
              {msg.toolName && <span className="tool-badge">{msg.toolName}</span>}
              {msg.content}
            </div>
            {msg.toolResult && (
              <div className="message-tool-result">
                <pre>{msg.toolResult}</pre>
              </div>
            )}
          </div>
        ))}
        <div ref={messagesEndRef} />
      </div>

      <form className="input-form" onSubmit={handleSubmit}>
        <input
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder={isConnected ? 'Waiting for response...' : 'Type a message'}
          disabled={isConnected}
          className="message-input"
        />
        <button
          type="submit"
          disabled={isConnected || !input.trim()}
          className="send-button"
        >
          Send
        </button>
      </form>

      {isConnected && (
        <div className="connection-status">
          <span className="status-dot"></span>
          Connected
        </div>
      )}
    </div>
  );
}
