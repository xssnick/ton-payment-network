import { InputHTMLAttributes } from "react";

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
    return (
        <input
            {...props}
            className={`px-3 py-2 border rounded-lg w-full text-sm ${props.className}`}
        />
    );
}